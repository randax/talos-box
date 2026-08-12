package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	// hubbleOwnershipAnnotation marks only chart objects that disappear when
	// Hubble is disabled. It lets state-free reconciliation remove precisely
	// its own optional resources without touching attendee-managed objects.
	hubbleOwnershipAnnotation = "talosbox.dev/hubble-owned"
	// announcementOwnershipAnnotation identifies BGP/L2 resources rendered by
	// talosbox so changing announcements can remove only its prior mode.
	announcementOwnershipAnnotation = "talosbox.dev/announcement-owned"
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
	if item.Hubble {
		candidates, err := ciliumHubbleObjects(item)
		if err != nil {
			return LoadBalancerResult{}, err
		}
		if err := validateHubbleOwnership(ctx, dynamicClient, mapper, candidates); err != nil {
			return LoadBalancerResult{}, err
		}
	}
	if err := applyAll(ctx, dynamicClient, mapper, chart); err != nil {
		return LoadBalancerResult{}, err
	}
	if !item.Hubble {
		candidates, err := ciliumHubbleObjects(item)
		if err != nil {
			return LoadBalancerResult{}, err
		}
		if err := deleteHubbleObjects(ctx, dynamicClient, mapper, candidates); err != nil {
			return LoadBalancerResult{}, err
		}
	}
	if err := waitForCiliumCRDs(ctx, dynamicClient, r.PollInterval); err != nil {
		return LoadBalancerResult{}, err
	}
	mapper.Reset()
	if item.LB {
		if err := deleteStaleCiliumAnnouncements(ctx, dynamicClient, mapper, item); err != nil {
			return LoadBalancerResult{}, err
		}
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
	if item.Hubble {
		if err := waitForHubble(ctx, clientset, r.PollInterval); err != nil {
			return LoadBalancerResult{}, err
		}
	}
	if !item.LB {
		return LoadBalancerResult{Narration: ciliumNarration(item, false)}, nil
	}
	vip, err := waitForProbe(ctx, clientset, item, r.PollInterval, r.HTTPClient)
	if err != nil {
		return LoadBalancerResult{}, err
	}
	return LoadBalancerResult{VIP: vip, Narration: ciliumNarration(item, true)}, nil
}

func renderCilium(item cluster.Cluster) ([]unstructured.Unstructured, error) {
	objects, err := renderCiliumForHubble(item, item.Hubble)
	if err != nil || !item.Hubble {
		return objects, err
	}
	candidates, err := ciliumHubbleObjects(item)
	if err != nil {
		return nil, err
	}
	return markHubbleObjects(objects, candidates), nil
}

func renderCiliumForHubble(item cluster.Cluster, hubble bool) ([]unstructured.Unstructured, error) {
	if item.CNI != cluster.CNICilium {
		return nil, errors.New("cilium rendering requires cni: cilium")
	}
	item.Hubble = hubble
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

func ciliumHubbleObjects(item cluster.Cluster) ([]unstructured.Unstructured, error) {
	enabled, err := renderCiliumForHubble(item, true)
	if err != nil {
		return nil, fmt.Errorf("render enabled Hubble objects: %w", err)
	}
	disabled, err := renderCiliumForHubble(item, false)
	if err != nil {
		return nil, fmt.Errorf("render disabled Hubble objects: %w", err)
	}
	disabledIDs := make(map[string]struct{}, len(disabled))
	for _, object := range disabled {
		disabledIDs[objectID(object)] = struct{}{}
	}
	candidates := make([]unstructured.Unstructured, 0, len(enabled))
	for _, object := range enabled {
		if _, unchanged := disabledIDs[objectID(object)]; !unchanged {
			candidates = append(candidates, object)
		}
	}
	return candidates, nil
}

func markHubbleObjects(objects, candidates []unstructured.Unstructured) []unstructured.Unstructured {
	ids := make(map[string]struct{}, len(candidates))
	for _, object := range candidates {
		ids[objectID(object)] = struct{}{}
	}
	marked := make([]unstructured.Unstructured, len(objects))
	for i, object := range objects {
		if _, hubble := ids[objectID(object)]; hubble {
			annotations := object.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[hubbleOwnershipAnnotation] = fieldManager
			object.SetAnnotations(annotations)
		}
		marked[i] = object
	}
	return marked
}

func objectID(object unstructured.Unstructured) string {
	return strings.Join([]string{object.GetAPIVersion(), object.GetKind(), object.GetNamespace(), object.GetName()}, "/")
}

func deleteHubbleObjects(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, candidates []unstructured.Unstructured) error {
	for _, candidate := range candidates {
		mapping, err := mapper.RESTMapping(candidate.GroupVersionKind().GroupKind(), candidate.GroupVersionKind().Version)
		if err != nil {
			return fmt.Errorf("map Hubble %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		var resource dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resource = client.Resource(mapping.Resource).Namespace(candidate.GetNamespace())
		} else {
			resource = client.Resource(mapping.Resource)
		}
		live, err := resource.Get(ctx, candidate.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("get Hubble %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		if live.GetAnnotations()[hubbleOwnershipAnnotation] != fieldManager {
			continue
		}
		if err := resource.Delete(ctx, candidate.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete Hubble %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
	}
	return nil
}

func validateHubbleOwnership(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, candidates []unstructured.Unstructured) error {
	for _, candidate := range candidates {
		mapping, err := mapper.RESTMapping(candidate.GroupVersionKind().GroupKind(), candidate.GroupVersionKind().Version)
		if err != nil {
			return fmt.Errorf("map Hubble %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		var resource dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resource = client.Resource(mapping.Resource).Namespace(candidate.GetNamespace())
		} else {
			resource = client.Resource(mapping.Resource)
		}
		live, err := resource.Get(ctx, candidate.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("get Hubble %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		if live.GetAnnotations()[hubbleOwnershipAnnotation] != fieldManager {
			return fmt.Errorf("refuse to adopt unmanaged Hubble %s %q", candidate.GetKind(), candidate.GetName())
		}
	}
	return nil
}

func staleCiliumAnnouncementObjects(item cluster.Cluster) ([]unstructured.Unstructured, error) {
	facts := manifestFacts(item)
	if item.BGP {
		return decodeObjects([]byte(manifests.L2Policy(facts)))
	}
	return decodeObjects([]byte(manifests.BGPPolicy(facts)))
}

func deleteStaleCiliumAnnouncements(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, item cluster.Cluster) error {
	candidates, err := staleCiliumAnnouncementObjects(item)
	if err != nil {
		return fmt.Errorf("render stale Cilium announcement objects: %w", err)
	}
	resources := make([]dynamic.ResourceInterface, 0, len(candidates))
	for _, candidate := range candidates {
		mapping, err := mapper.RESTMapping(candidate.GroupVersionKind().GroupKind(), candidate.GroupVersionKind().Version)
		if err != nil {
			return fmt.Errorf("map stale Cilium %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		var resource dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resource = client.Resource(mapping.Resource).Namespace(candidate.GetNamespace())
		} else {
			resource = client.Resource(mapping.Resource)
		}
		live, err := resource.Get(ctx, candidate.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			resources = append(resources, nil)
			continue
		}
		if err != nil {
			return fmt.Errorf("get stale Cilium %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		if !announcementOwnedByTalosbox(live) {
			return fmt.Errorf("refuse to remove unmanaged Cilium %s %q", candidate.GetKind(), candidate.GetName())
		}
		resources = append(resources, resource)
	}
	for i, candidate := range candidates {
		if resources[i] == nil {
			continue
		}
		if err := resources[i].Delete(ctx, candidate.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale Cilium %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
	}
	return nil
}

func announcementOwnedByTalosbox(object *unstructured.Unstructured) bool {
	if object.GetAnnotations()[announcementOwnershipAnnotation] == fieldManager {
		return true
	}
	for _, field := range object.GetManagedFields() {
		if field.Manager == fieldManager {
			return true
		}
	}
	return false
}

func ciliumNarration(item cluster.Cluster, loadBalancer bool) []string {
	narration := []string{
		"≈ helm template cilium cilium/cilium --version " + ciliumChartVersion + " -n " + ciliumNamespace + " | kubectl apply --server-side -f -",
	}
	if loadBalancer {
		narration = append(narration, "≈ kubectl apply --server-side -f - # Cilium LB-IPAM/L2 pool and VIP probe")
	}
	if item.Hubble {
		narration = append(narration, "≈ kubectl port-forward -n kube-system service/hubble-ui 12000:80 # Hubble UI at http://localhost:12000")
	}
	return narration
}

func ciliumExtras(f manifests.Facts) string {
	if f.BGP {
		return manifests.LBPool(f) + "---\n" + manifests.BGPPolicy(f)
	}
	return manifests.LBPool(f) + "---\n" + manifests.L2Policy(f)
}

// CiliumConverged observes the Cilium workload and the exact announcement
// desired set. It lets a fully converged L2 cluster skip a costly full-chart
// SSA pass, while stale resources from an interrupted BGP -> L2 change force
// the normal reconciler to clean them up on the next `tbx up`.
func CiliumConverged(ctx context.Context, kubeconfig []byte, item cluster.Cluster) error {
	transport, server, err := kubeTransport(kubeconfig)
	if err != nil {
		return err
	}
	defer transport.CloseIdleConnections()
	for _, workload := range []struct {
		path string
		kind string
	}{
		{"/apis/apps/v1/namespaces/kube-system/deployments/cilium-operator", "deployment"},
		{"/apis/apps/v1/namespaces/kube-system/daemonsets/cilium", "daemonset"},
		{"/apis/apps/v1/namespaces/kube-system/daemonsets/cilium-envoy", "daemonset"},
	} {
		if err := ciliumWorkloadReady(ctx, transport, server, workload.path, workload.kind); err != nil {
			return err
		}
	}
	if item.LB {
		if err := ciliumOwnedObjectState(ctx, transport, server, "/apis/cilium.io/v2/ciliumloadbalancerippools/"+item.Name+"-pool", announcementOwnershipAnnotation, fieldManager); err != nil {
			return err
		}
		if item.BGP {
			for _, path := range ciliumBGPPaths(item.Name) {
				if err := ciliumAnnouncementState(ctx, transport, server, path); err != nil {
					return err
				}
			}
			if err := ciliumObjectState(ctx, transport, server, ciliumL2Path(item.Name), false); err != nil {
				return err
			}
		} else {
			if err := ciliumAnnouncementState(ctx, transport, server, ciliumL2Path(item.Name)); err != nil {
				return err
			}
			for _, path := range ciliumBGPPaths(item.Name) {
				if err := ciliumObjectState(ctx, transport, server, path, false); err != nil {
					return err
				}
			}
		}
		if err := ciliumOwnedWorkloadReady(ctx, transport, server, ciliumProbeDeploymentPath(), "deployment", "talosbox.dev/managed", "true"); err != nil {
			return err
		}
		if err := ciliumOwnedObjectState(ctx, transport, server, ciliumProbeServicePath(), "talosbox.dev/managed", "true"); err != nil {
			return err
		}
	} else {
		if err := ciliumObjectState(ctx, transport, server, "/apis/cilium.io/v2/ciliumloadbalancerippools/"+item.Name+"-pool", false); err != nil {
			return err
		}
		if err := ciliumObjectState(ctx, transport, server, ciliumL2Path(item.Name), false); err != nil {
			return err
		}
		for _, path := range ciliumBGPPaths(item.Name) {
			if err := ciliumObjectState(ctx, transport, server, path, false); err != nil {
				return err
			}
		}
		if err := ciliumObjectState(ctx, transport, server, ciliumProbeDeploymentPath(), false); err != nil {
			return err
		}
		if err := ciliumObjectState(ctx, transport, server, ciliumProbeServicePath(), false); err != nil {
			return err
		}
	}
	return ciliumHubbleConverged(ctx, transport, server, item)
}

func ciliumL2Path(name string) string {
	return "/apis/cilium.io/v2alpha1/ciliuml2announcementpolicies/" + name + "-l2"
}

func ciliumBGPPaths(name string) []string {
	return []string{
		"/apis/cilium.io/v2/ciliumbgpclusterconfigs/" + name + "-bgp",
		"/apis/cilium.io/v2/ciliumbgppeerconfigs/" + name + "-bgp-peer",
		"/apis/cilium.io/v2/ciliumbgpadvertisements/" + name + "-bgp-advertisement",
	}
}

func ciliumProbeDeploymentPath() string {
	return "/apis/apps/v1/namespaces/" + probeNamespace + "/deployments/lb-probe"
}

func ciliumProbeServicePath() string {
	return "/api/v1/namespaces/" + probeNamespace + "/services/lb-probe"
}

func ciliumObjectState(ctx context.Context, transport http.RoundTripper, server, path string, present bool) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server+path, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if present && response.StatusCode != http.StatusOK {
		return fmt.Errorf("cilium desired object %s: %s", path, response.Status)
	}
	if !present && response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("stale Cilium object %s: %s", path, response.Status)
	}
	return nil
}

func ciliumOwnedObjectState(ctx context.Context, transport http.RoundTripper, server, path, key, value string) error {
	response, err := ciliumGet(ctx, transport, server, path)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("cilium desired object %s: %s", path, response.Status)
	}
	var object struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
			Labels      map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(response.Body).Decode(&object); err != nil {
		return fmt.Errorf("decode Cilium object %s: %w", path, err)
	}
	if object.Metadata.Annotations[key] != value && object.Metadata.Labels[key] != value {
		return fmt.Errorf("cilium desired object %s is not owned by talosbox", path)
	}
	return nil
}

// ciliumAnnouncementState additionally checks the ownership annotation. The
// resource names are stable, but ownership prevents a coincidental attendee
// resource from being treated as talosbox's declarative announcement set.
func ciliumAnnouncementState(ctx context.Context, transport http.RoundTripper, server, path string) error {
	response, err := ciliumGet(ctx, transport, server, path)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("cilium desired announcement %s: %s", path, response.Status)
	}
	var object struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(response.Body).Decode(&object); err != nil {
		return fmt.Errorf("decode Cilium announcement %s: %w", path, err)
	}
	if object.Metadata.Annotations["talosbox.dev/announcement-owned"] != "talosbox" {
		return fmt.Errorf("cilium desired announcement %s is not owned by talosbox", path)
	}
	return nil
}

func ciliumGet(ctx context.Context, transport http.RoundTripper, server, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server+path, nil)
	if err != nil {
		return nil, err
	}
	return (&http.Client{Transport: transport}).Do(request)
}

func ciliumOwnedWorkloadReady(ctx context.Context, transport http.RoundTripper, server, path, kind, key, value string) error {
	if err := ciliumWorkloadReady(ctx, transport, server, path, kind); err != nil {
		return err
	}
	return ciliumOwnedObjectState(ctx, transport, server, path, key, value)
}

func ciliumHubbleConverged(ctx context.Context, transport http.RoundTripper, server string, item cluster.Cluster) error {
	candidates, err := ciliumHubbleObjects(item)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		path, err := ciliumObjectPath(candidate)
		if err != nil {
			return err
		}
		if !item.Hubble {
			if err := ciliumObjectState(ctx, transport, server, path, false); err != nil {
				return err
			}
			continue
		}
		if err := ciliumOwnedObjectState(ctx, transport, server, path, hubbleOwnershipAnnotation, fieldManager); err != nil {
			return err
		}
		if candidate.GetKind() == "Deployment" {
			if err := ciliumWorkloadReady(ctx, transport, server, path, "deployment"); err != nil {
				return err
			}
		}
	}
	return nil
}

func ciliumObjectPath(object unstructured.Unstructured) (string, error) {
	version := object.GroupVersionKind().Version
	if version == "" || object.GetKind() == "" || object.GetName() == "" {
		return "", fmt.Errorf("invalid Cilium object identity %q", objectID(object))
	}
	group := object.GroupVersionKind().Group
	prefix := "/api/" + version
	if group != "" {
		prefix = "/apis/" + group + "/" + version
	}
	if object.GetNamespace() != "" {
		prefix += "/namespaces/" + object.GetNamespace()
	}
	return prefix + "/" + ciliumResourceName(object.GetKind()) + "/" + object.GetName(), nil
}

func ciliumResourceName(kind string) string {
	lower := strings.ToLower(kind)
	if strings.HasSuffix(lower, "s") {
		return lower + "es"
	}
	if strings.HasSuffix(lower, "y") {
		return strings.TrimSuffix(lower, "y") + "ies"
	}
	return lower + "s"
}

func ciliumWorkloadReady(ctx context.Context, transport http.RoundTripper, server, path, kind string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server+path, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return fmt.Errorf("cilium %s %s: %s", kind, path, response.Status)
	}
	var workload struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Status struct {
			ObservedGeneration     int64 `json:"observedGeneration"`
			ReadyReplicas          int32 `json:"readyReplicas"`
			AvailableReplicas      int32 `json:"availableReplicas"`
			DesiredNumberScheduled int32 `json:"desiredNumberScheduled"`
			NumberReady            int32 `json:"numberReady"`
		} `json:"status"`
	}
	err = json.NewDecoder(response.Body).Decode(&workload)
	_ = response.Body.Close()
	if err != nil {
		return fmt.Errorf("decode Cilium %s %s: %w", kind, path, err)
	}
	if workload.Status.ObservedGeneration < workload.Metadata.Generation {
		return fmt.Errorf("cilium %s %s has not observed its generation", kind, path)
	}
	if kind == "deployment" && (workload.Status.ReadyReplicas < 1 || workload.Status.AvailableReplicas < 1) {
		return fmt.Errorf("cilium deployment %s is not Ready", path)
	}
	if kind == "daemonset" && (workload.Status.DesiredNumberScheduled < 1 || workload.Status.NumberReady < workload.Status.DesiredNumberScheduled) {
		return fmt.Errorf("cilium daemonset %s is not Ready", path)
	}
	return nil
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

func waitForHubble(ctx context.Context, client kubernetes.Interface, interval time.Duration) error {
	return poll(ctx, interval, func(ctx context.Context) error {
		for _, name := range []string{"hubble-relay", "hubble-ui"} {
			deployment, err := client.AppsV1().Deployments(ciliumNamespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil || !deploymentReady(deployment) {
				return fmt.Errorf("hubble %s is not ready", name)
			}
		}
		return nil
	})
}

func waitForCiliumCRDs(ctx context.Context, client dynamic.Interface, interval time.Duration) error {
	names := []string{
		"ciliumloadbalancerippools.cilium.io",
		"ciliuml2announcementpolicies.cilium.io",
		"ciliumbgpclusterconfigs.cilium.io",
		"ciliumbgppeerconfigs.cilium.io",
		"ciliumbgpadvertisements.cilium.io",
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
