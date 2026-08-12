package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/manifests"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/releaseutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	ciliumChartVersion = "1.19.6"
	ciliumChartSHA256  = "21c43cf53841f9ab0375047d95aa4c64051ea52bbd2c679416e6408f5f1c9179"
	ciliumNamespace    = "kube-system"
)

//go:embed assets/cilium-1.19.6.tgz
var ciliumChart []byte

// CiliumReconciler renders the bundled Cilium chart locally and applies it
// through the same client-go server-side-apply channel as MetalLB. It does not
// invoke Helm or kubectl, and neither the chart nor its manifests are fetched
// by a guest.
type CiliumReconciler struct {
	PollInterval time.Duration
	HTTPClient   *http.Client
}

// Reconcile installs Cilium before waiting for Kubernetes Nodes to become
// Ready: cni.name none intentionally leaves them NotReady until this chart's
// daemonset is running.
func (r CiliumReconciler) Reconcile(ctx context.Context, item cluster.Cluster, kubeconfig []byte) (LoadBalancerResult, error) {
	objects, err := renderCilium(item)
	if err != nil {
		return LoadBalancerResult{}, err
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return LoadBalancerResult{}, fmt.Errorf("parse kubeconfig for Cilium apply: %w", err)
	}
	return r.reconcile(ctx, item, config, objects)
}

func (r CiliumReconciler) reconcile(ctx context.Context, item cluster.Cluster, config *rest.Config, objects []unstructured.Unstructured) (LoadBalancerResult, error) {
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

	namespaces, chart, extras, probe := partitionCiliumObjects(objects)
	if err := applyAll(ctx, dynamicClient, mapper, namespaces); err != nil {
		return LoadBalancerResult{}, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, chart); err != nil {
		return LoadBalancerResult{}, err
	}
	if err := waitForCiliumCRDs(ctx, dynamicClient, r.PollInterval, item.BGP); err != nil {
		return LoadBalancerResult{}, err
	}
	mapper.Reset()
	if item.LB {
		if err := applyAll(ctx, dynamicClient, mapper, extras); err != nil {
			return LoadBalancerResult{}, err
		}
		if err := applyAll(ctx, dynamicClient, mapper, probe); err != nil {
			return LoadBalancerResult{}, err
		}
	}
	if err := waitForCilium(ctx, clientset, r.PollInterval); err != nil {
		return LoadBalancerResult{}, err
	}
	if !item.LB {
		return LoadBalancerResult{Narration: []string{
			"≈ helm template cilium cilium/cilium --version " + ciliumChartVersion + " -n " + ciliumNamespace + " | kubectl apply --server-side -f -",
		}}, nil
	}
	vip, err := waitForProbe(ctx, clientset, item, r.PollInterval, r.HTTPClient)
	if err != nil {
		return LoadBalancerResult{}, err
	}
	return LoadBalancerResult{VIP: vip, Narration: []string{
		"≈ helm template cilium cilium/cilium --version " + ciliumChartVersion + " -n " + ciliumNamespace + " | kubectl apply --server-side -f -",
		"≈ kubectl apply --server-side -f - # Cilium LB-IPAM/L2 pool and VIP probe",
	}}, nil
}

func renderCilium(item cluster.Cluster) ([]unstructured.Unstructured, error) {
	if item.CNI != cluster.CNICilium {
		return nil, errors.New("cilium rendering requires cni: cilium")
	}
	if actual := fmt.Sprintf("%x", sha256.Sum256(ciliumChart)); actual != ciliumChartSHA256 {
		return nil, fmt.Errorf("embedded Cilium chart checksum = %s, want %s", actual, ciliumChartSHA256)
	}
	chart, err := loader.LoadArchive(bytes.NewReader(ciliumChart))
	if err != nil {
		return nil, fmt.Errorf("load embedded Cilium chart: %w", err)
	}
	if chart.Metadata.Version != ciliumChartVersion {
		return nil, fmt.Errorf("embedded Cilium chart version = %s, want %s", chart.Metadata.Version, ciliumChartVersion)
	}
	values, err := chartutil.ReadValues([]byte(manifests.CiliumValues(manifestFacts(item))))
	if err != nil {
		return nil, fmt.Errorf("decode Cilium values: %w", err)
	}
	renderValues, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{Name: "cilium", Namespace: ciliumNamespace}, chartutil.DefaultCapabilities)
	if err != nil {
		return nil, fmt.Errorf("prepare Cilium render values: %w", err)
	}
	rendered, err := (engine.Engine{}).Render(chart, renderValues)
	if err != nil {
		return nil, fmt.Errorf("render embedded Cilium chart: %w", err)
	}
	for name := range rendered {
		if strings.HasSuffix(name, "NOTES.txt") {
			delete(rendered, name)
		}
	}
	_, sorted, err := releaseutil.SortManifests(rendered, chartutil.DefaultVersionSet, releaseutil.InstallOrder)
	if err != nil {
		return nil, fmt.Errorf("sort rendered Cilium chart: %w", err)
	}
	var result []unstructured.Unstructured
	for _, manifest := range sorted {
		objects, err := decodeObjects([]byte(manifest.Content))
		if err != nil {
			return nil, fmt.Errorf("decode rendered Cilium %s: %w", manifest.Name, err)
		}
		result = append(result, objects...)
	}
	if !item.LB {
		return result, nil
	}
	extras, err := decodeObjects([]byte(ciliumExtras(manifestFacts(item))))
	if err != nil {
		return nil, fmt.Errorf("decode Cilium LB extras: %w", err)
	}
	probe, err := decodeObjects([]byte(ciliumProbe(item)))
	if err != nil {
		return nil, fmt.Errorf("decode Cilium VIP probe: %w", err)
	}
	return append(append(result, extras...), probe...), nil
}

func ciliumExtras(f manifests.Facts) string {
	if f.BGP {
		return manifests.LBPool(f) + "---\n" + manifests.BGPPolicy(f)
	}
	return manifests.LBPool(f) + "---\n" + manifests.L2Policy(f)
}

func ciliumProbe(item cluster.Cluster) string {
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
  annotations:
    lbipam.cilium.io/ips: 172.30.%d.200
spec:
  type: LoadBalancer
  selector:
    app: talosbox-lb-probe
  ports:
    - port: 80
      targetPort: 8080
`, probeNamespace, probeNamespace, probeNamespace, item.SubnetIndex)
}

func partitionCiliumObjects(objects []unstructured.Unstructured) (namespaces, chart, extras, probe []unstructured.Unstructured) {
	for _, object := range objects {
		switch object.GetKind() {
		case "CustomResourceDefinition":
			chart = append(chart, object)
		case "CiliumLoadBalancerIPPool", "CiliumL2AnnouncementPolicy", "CiliumBGPClusterConfig", "CiliumBGPPeerConfig", "CiliumBGPAdvertisement":
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
	return namespaces, chart, extras, probe
}

func waitForCilium(ctx context.Context, client kubernetes.Interface, interval time.Duration) error {
	return poll(ctx, interval, func(ctx context.Context) error {
		operator, err := client.AppsV1().Deployments(ciliumNamespace).Get(ctx, "cilium-operator", metav1.GetOptions{})
		if err != nil || !deploymentReady(operator) {
			return errors.New("cilium operator is not ready")
		}
		agent, err := client.AppsV1().DaemonSets(ciliumNamespace).Get(ctx, "cilium", metav1.GetOptions{})
		if err != nil || !daemonSetReady(agent) {
			return errors.New("cilium agent is not ready")
		}
		envoy, err := client.AppsV1().DaemonSets(ciliumNamespace).Get(ctx, "cilium-envoy", metav1.GetOptions{})
		if err != nil || !daemonSetReady(envoy) {
			return errors.New("cilium envoy is not ready")
		}
		return nil
	})
}

func waitForCiliumCRDs(ctx context.Context, client dynamic.Interface, interval time.Duration, bgp bool) error {
	names := []string{
		"ciliumloadbalancerippools.cilium.io",
		"ciliuml2announcementpolicies.cilium.io",
	}
	if bgp {
		names = append(names,
			"ciliumbgpclusterconfigs.cilium.io",
			"ciliumbgppeerconfigs.cilium.io",
			"ciliumbgpadvertisements.cilium.io",
		)
	}
	resource := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	return poll(ctx, interval, func(ctx context.Context) error {
		for _, name := range names {
			live, err := client.Resource(resource).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			conditions, found, err := unstructured.NestedSlice(live.Object, "status", "conditions")
			if err != nil || !found || !conditionTrue(conditions, "Established") {
				return fmt.Errorf("cilium CRD %q is not established", name)
			}
		}
		return nil
	})
}
