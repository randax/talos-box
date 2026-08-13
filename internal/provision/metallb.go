package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/manifests"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/releaseutil"
	appsv1 "k8s.io/api/apps/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	metalLBChartVersion = "0.16.1"
	metalLBChartSHA256  = "fb06bb584fcb7856f15733b2a6a2aff5b61b5c350687e341c163ae24a5938adc"
	fieldManager        = "talosbox"
	metalLBNamespace    = "metallb-system"
	probeNamespace      = "talosbox-system"
)

//go:embed assets/metallb-0.16.1.tgz
var metalLBChart []byte

// LoadBalancerResult is the verified public endpoint and its manual-equivalent
// narration after the host-side reconcile completes.
type LoadBalancerResult struct {
	VIP       string
	Narration []string
}

// MetalLBReconciler is the host-side Helm-render/client-go-SSA implementation
// of flannel LoadBalancer support. It deliberately has no helm or kubectl
// subprocess surface and never asks a guest to download manifests.
type MetalLBReconciler struct {
	PollInterval time.Duration
	HTTPClient   *http.Client
}

// LiveVIP returns the probe VIP only after both Kubernetes has assigned it and
// the host can receive a successful response through the L2 path.
func LiveVIP(ctx context.Context, item cluster.Cluster, kubeconfig []byte) (string, bool) {
	if item.CNI != cluster.CNIFlannel || !item.LB {
		return "", false
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return "", false
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", false
	}
	service, err := clientset.CoreV1().Services(probeNamespace).Get(ctx, "lb-probe", metav1.GetOptions{})
	if err != nil || len(service.Status.LoadBalancer.Ingress) != 1 {
		return "", false
	}
	vip := service.Status.LoadBalancer.Ingress[0].IP
	if vip != fmt.Sprintf("172.30.%d.200", item.SubnetIndex) {
		return "", false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+vip+"/", nil)
	if err != nil {
		return "", false
	}
	response, err := vipHTTPClient(nil).Do(request)
	if err != nil {
		return "", false
	}
	respondedOK := response.StatusCode == http.StatusOK
	if err := response.Body.Close(); err != nil {
		return "", false
	}
	return vip, respondedOK
}

func (r MetalLBReconciler) Reconcile(ctx context.Context, item cluster.Cluster, kubeconfig []byte) (LoadBalancerResult, error) {
	objects, err := renderMetalLB(item)
	if err != nil {
		return LoadBalancerResult{}, err
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return LoadBalancerResult{}, fmt.Errorf("parse kubeconfig for Kubernetes apply: %w", err)
	}
	return r.reconcile(ctx, item, config, objects)
}

func (r MetalLBReconciler) reconcile(ctx context.Context, item cluster.Cluster, config *rest.Config, objects []unstructured.Unstructured) (LoadBalancerResult, error) {
	if r.PollInterval <= 0 {
		r.PollInterval = time.Second
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return LoadBalancerResult{}, fmt.Errorf("create Kubernetes discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return LoadBalancerResult{}, fmt.Errorf("create Kubernetes apply client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return LoadBalancerResult{}, fmt.Errorf("create Kubernetes readiness client: %w", err)
	}

	namespaces, chart, crds, extras, probe := partitionMetalLBObjects(objects)
	if err := applyAll(ctx, dynamicClient, mapper, namespaces); err != nil {
		return LoadBalancerResult{}, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, crds); err != nil {
		return LoadBalancerResult{}, err
	}
	if err := waitForCRDs(ctx, dynamicClient, mapper, crds, "MetalLB", r.PollInterval); err != nil {
		return LoadBalancerResult{}, err
	}
	mapper.Reset()
	if err := applyAll(ctx, dynamicClient, mapper, chart); err != nil {
		return LoadBalancerResult{}, err
	}
	if err := waitForMetalLB(ctx, clientset, r.PollInterval); err != nil {
		return LoadBalancerResult{}, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, extras); err != nil {
		return LoadBalancerResult{}, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, probe); err != nil {
		return LoadBalancerResult{}, err
	}
	vip, err := waitForProbe(ctx, clientset, item, r.PollInterval, r.HTTPClient)
	if err != nil {
		return LoadBalancerResult{}, err
	}
	return LoadBalancerResult{VIP: vip, Narration: []string{
		"≈ helm template metallb metallb/metallb --version " + metalLBChartVersion + " -n " + metalLBNamespace + " | kubectl apply --server-side -f -",
		"≈ kubectl apply --server-side -f - # MetalLB L2 pool and VIP probe",
	}}, nil
}

func renderMetalLB(item cluster.Cluster) ([]unstructured.Unstructured, error) {
	if item.CNI != cluster.CNIFlannel || !item.LB {
		return nil, errors.New("MetalLB rendering requires flannel with lb: true")
	}
	if actual := fmt.Sprintf("%x", sha256.Sum256(metalLBChart)); actual != metalLBChartSHA256 {
		return nil, fmt.Errorf("embedded MetalLB chart checksum = %s, want %s", actual, metalLBChartSHA256)
	}
	chart, err := loader.LoadArchive(bytes.NewReader(metalLBChart))
	if err != nil {
		return nil, fmt.Errorf("load embedded MetalLB chart: %w", err)
	}
	if chart.Metadata.Version != metalLBChartVersion {
		return nil, fmt.Errorf("embedded MetalLB chart version = %s, want %s", chart.Metadata.Version, metalLBChartVersion)
	}
	values, err := chartutil.ReadValues([]byte(manifests.MetalLBValues(manifestFacts(item))))
	if err != nil {
		return nil, fmt.Errorf("decode MetalLB values: %w", err)
	}
	renderValues, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{Name: "metallb", Namespace: metalLBNamespace}, chartutil.DefaultCapabilities)
	if err != nil {
		return nil, fmt.Errorf("prepare MetalLB render values: %w", err)
	}
	rendered, err := (engine.Engine{}).Render(chart, renderValues)
	if err != nil {
		return nil, fmt.Errorf("render embedded MetalLB chart: %w", err)
	}
	for name := range rendered {
		if strings.HasSuffix(name, "NOTES.txt") {
			delete(rendered, name)
		}
	}
	_, sorted, err := releaseutil.SortManifests(rendered, chartutil.DefaultVersionSet, releaseutil.InstallOrder)
	if err != nil {
		return nil, fmt.Errorf("sort rendered MetalLB chart: %w", err)
	}
	var result []unstructured.Unstructured
	for _, manifest := range sorted {
		objects, err := decodeObjects([]byte(manifest.Content))
		if err != nil {
			return nil, fmt.Errorf("decode rendered MetalLB %s: %w", manifest.Name, err)
		}
		for _, object := range objects {
			// The packaged chart retains the disabled frr-k8s subchart's CRD
			// templates. Keeping them would install BGP-only API surface on an
			// intentionally L2-only cluster.
			crdGroup, _, _ := unstructured.NestedString(object.Object, "spec", "group")
			if crdGroup == "frrk8s.metallb.io" {
				continue
			}
			result = append(result, object)
		}
	}
	result = append([]unstructured.Unstructured{{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name":   metalLBNamespace,
			"labels": map[string]any{"talosbox.dev/managed": "true"},
		},
	}}}, result...)
	extras, err := decodeObjects([]byte(manifests.MetalLBExtras(manifestFacts(item))))
	if err != nil {
		return nil, fmt.Errorf("decode MetalLB extras: %w", err)
	}
	probe, err := decodeObjects([]byte(metalLBProbe(item)))
	if err != nil {
		return nil, fmt.Errorf("decode MetalLB VIP probe: %w", err)
	}
	return append(append(result, extras...), probe...), nil
}

func manifestFacts(item cluster.Cluster) manifests.Facts {
	return manifests.Facts{Cluster: item.Name, SubnetIndex: item.SubnetIndex, BGP: item.BGP}
}

func metalLBProbe(item cluster.Cluster) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    talosbox.dev/managed: "true"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: lb-probe
  namespace: %s
  labels:
    talosbox.dev/managed: "true"
spec:
  replicas: 1
  selector:
    matchLabels:
      app: talosbox-lb-probe
  template:
    metadata:
      labels:
        app: talosbox-lb-probe
        talosbox.dev/managed: "true"
    spec:
      containers:
        - name: server
          image: registry.k8s.io/e2e-test-images/agnhost:2.53
          args: ["netexec", "--http-port=8080"]
          ports:
            - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: lb-probe
  namespace: %s
  labels:
    talosbox.dev/managed: "true"
spec:
  type: LoadBalancer
  loadBalancerIP: 172.30.%d.200
  selector:
    app: talosbox-lb-probe
  ports:
    - port: 80
      targetPort: 8080
`, probeNamespace, probeNamespace, probeNamespace, item.SubnetIndex)
}

func decodeObjects(data []byte) ([]unstructured.Unstructured, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []unstructured.Unstructured
	for {
		var raw map[string]any
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return objects, nil
			}
			return nil, err
		}
		if len(raw) == 0 {
			continue
		}
		object := unstructured.Unstructured{Object: raw}
		if object.GetAPIVersion() == "" || object.GetKind() == "" {
			return nil, errors.New("object lacks apiVersion or kind")
		}
		objects = append(objects, object)
	}
}

func partitionMetalLBObjects(objects []unstructured.Unstructured) (namespaces, chart, crds, extras, probe []unstructured.Unstructured) {
	for _, object := range objects {
		switch object.GetKind() {
		case "CustomResourceDefinition":
			crds = append(crds, object)
		case "IPAddressPool", "L2Advertisement":
			extras = append(extras, object)
		case "Namespace":
			if object.GetName() == probeNamespace {
				probe = append(probe, object)
			} else {
				namespaces = append(namespaces, object)
			}
		case "Deployment", "Service":
			if object.GetNamespace() == probeNamespace {
				probe = append(probe, object)
			} else {
				chart = append(chart, object)
			}
		default:
			chart = append(chart, object)
		}
	}
	return namespaces, chart, crds, extras, probe
}

func applyAll(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, objects []unstructured.Unstructured) error {
	for _, object := range objects {
		mapping, err := mapper.RESTMapping(object.GroupVersionKind().GroupKind(), object.GroupVersionKind().Version)
		if err != nil {
			return fmt.Errorf("map %s %q: %w", object.GetKind(), object.GetName(), err)
		}
		data, err := runtime.Encode(unstructured.UnstructuredJSONScheme, &object)
		if err != nil {
			return fmt.Errorf("encode %s %q for apply: %w", object.GetKind(), object.GetName(), err)
		}
		var resource dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resource = client.Resource(mapping.Resource).Namespace(object.GetNamespace())
		} else {
			resource = client.Resource(mapping.Resource)
		}
		if _, err := resource.Patch(ctx, object.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
			return fmt.Errorf("server-side apply %s %q: %w", object.GetKind(), object.GetName(), err)
		}
	}
	return nil
}

func waitForCRDs(ctx context.Context, client dynamic.Interface, mapper *restmapper.DeferredDiscoveryRESTMapper, crds []unstructured.Unstructured, component string, interval time.Duration) error {
	if len(crds) == 0 {
		return fmt.Errorf("embedded %s chart contains no CRDs", component)
	}
	mapping, err := mapper.RESTMapping(schema.GroupKind{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}, "v1")
	if err != nil {
		return fmt.Errorf("map %s CRDs: %w", component, err)
	}
	return poll(ctx, interval, func(ctx context.Context) error {
		for _, crd := range crds {
			live, err := client.Resource(mapping.Resource).Get(ctx, crd.GetName(), metav1.GetOptions{})
			if err != nil {
				return err
			}
			conditions, found, err := unstructured.NestedSlice(live.Object, "status", "conditions")
			if err != nil || !found {
				return fmt.Errorf("CRD %q is not Established", crd.GetName())
			}
			if !conditionTrue(conditions, "Established") {
				return fmt.Errorf("CRD %q is not Established", crd.GetName())
			}
		}
		return nil
	})
}

func conditionTrue(conditions []any, name string) bool {
	for _, condition := range conditions {
		value, ok := condition.(map[string]any)
		if ok && value["type"] == name && value["status"] == "True" {
			return true
		}
	}
	return false
}

func waitForMetalLB(ctx context.Context, client kubernetes.Interface, interval time.Duration) error {
	return poll(ctx, interval, func(ctx context.Context) error {
		controller, err := client.AppsV1().Deployments(metalLBNamespace).Get(ctx, "metallb-controller", metav1.GetOptions{})
		if err != nil {
			return err
		}
		speaker, err := client.AppsV1().DaemonSets(metalLBNamespace).Get(ctx, "metallb-speaker", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !deploymentReady(controller) || !daemonSetReady(speaker) {
			return errors.New("MetalLB controller or speaker is not Ready")
		}
		endpointSlices, err := client.DiscoveryV1().EndpointSlices(metalLBNamespace).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=metallb-webhook-service"})
		if err != nil || !endpointSlicesReady(endpointSlices.Items) {
			return errors.New("MetalLB webhook endpoint is not Ready")
		}
		return nil
	})
}

func waitForProbe(ctx context.Context, client kubernetes.Interface, item cluster.Cluster, interval time.Duration, httpClient *http.Client) (string, error) {
	var vip string
	err := poll(ctx, interval, func(ctx context.Context) error {
		service, err := client.CoreV1().Services(probeNamespace).Get(ctx, "lb-probe", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if len(service.Status.LoadBalancer.Ingress) != 1 || service.Status.LoadBalancer.Ingress[0].IP == "" {
			return errors.New("LoadBalancer probe has no assigned VIP")
		}
		vip = service.Status.LoadBalancer.Ingress[0].IP
		want := fmt.Sprintf("172.30.%d.200", item.SubnetIndex)
		if vip != want {
			return fmt.Errorf("LoadBalancer probe VIP = %s, want %s", vip, want)
		}
		if !deploymentReadyForProbe(ctx, client) {
			return errors.New("LoadBalancer probe deployment is not Ready")
		}
		client := vipHTTPClient(httpClient)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+vip+"/", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		if err := response.Body.Close(); err != nil {
			return fmt.Errorf("close LoadBalancer probe response: %w", err)
		}
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("LoadBalancer probe response = %s", response.Status)
		}
		return nil
	})
	return vip, err
}

func deploymentReady(deployment *appsv1.Deployment) bool {
	return deployment.Generation == deployment.Status.ObservedGeneration && deployment.Status.ReadyReplicas >= 1 && deployment.Status.AvailableReplicas >= 1
}

func daemonSetReady(daemonSet *appsv1.DaemonSet) bool {
	return daemonSet.Generation == daemonSet.Status.ObservedGeneration && daemonSet.Status.DesiredNumberScheduled > 0 && daemonSet.Status.NumberReady == daemonSet.Status.DesiredNumberScheduled
}

func endpointSlicesReady(endpointSlices []discoveryv1.EndpointSlice) bool {
	for _, endpointSlice := range endpointSlices {
		if len(endpointSlice.Ports) == 0 {
			continue
		}
		for _, endpoint := range endpointSlice.Endpoints {
			if len(endpoint.Addresses) > 0 && endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready {
				return true
			}
		}
	}
	return false
}

func deploymentReadyForProbe(ctx context.Context, client kubernetes.Interface) bool {
	deployment, err := client.AppsV1().Deployments(probeNamespace).Get(ctx, "lb-probe", metav1.GetOptions{})
	return err == nil && deploymentReady(deployment)
}

func vipHTTPClient(httpClient *http.Client) *http.Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Second}
	}
	client := *httpClient
	switch transport := client.Transport.(type) {
	case nil:
		client.Transport = defaultProxylessTransport()
	case *http.Transport:
		cloned := transport.Clone()
		cloned.Proxy = nil
		client.Transport = cloned
	}
	return &client
}

func defaultProxylessTransport() *http.Transport {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{Proxy: nil}
	}
	cloned := transport.Clone()
	cloned.Proxy = nil
	return cloned
}

func poll(ctx context.Context, interval time.Duration, check func(context.Context) error) error {
	var lastErr error
	for {
		if err := check(ctx); err == nil {
			return nil
		} else {
			var terminal terminalError
			if errors.As(err, &terminal) {
				return terminal.err
			}
			lastErr = err
		}
		if err := wait(ctx, interval); err != nil {
			if lastErr == nil {
				return err
			}
			return errors.Join(err, lastErr)
		}
	}
}

type terminalError struct{ err error }

func (err terminalError) Error() string { return err.err.Error() }

func (err terminalError) Unwrap() error { return err.err }

func terminal(err error) error {
	if err == nil {
		return nil
	}
	return terminalError{err: err}
}
