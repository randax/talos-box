package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/manifests"
	"github.com/randax/talos-box/internal/shellquote"
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
	// MirrorOffline records that the registry mirror is serving from cache
	// only. It changes nothing about the apply path; it is the one piece of
	// context that makes a control plane which never starts diagnosable
	// (see annotateAPIServerTimeout).
	MirrorOffline          bool
	LoadIngressPKI         func(cluster.Cluster) (ingressPKI, error)
	NewHTTPSProbeTransport func(*tls.Config, func(context.Context, string, string) (net.Conn, error)) http.RoundTripper
}

// Reconcile installs Cilium before waiting for Kubernetes Nodes to become
// Ready: cni.name none intentionally leaves them NotReady until this chart's
// daemonset is running.
func (r CiliumReconciler) Reconcile(ctx context.Context, item cluster.Cluster, kubeconfig []byte) (LoadBalancerResult, error) {
	objects, err := renderCilium(item)
	if err != nil {
		return LoadBalancerResult{}, err
	}
	var ingressPKI ingressPKI
	if item.LB {
		loader := r.LoadIngressPKI
		if loader == nil {
			loader = loadIngressPKIForCluster
		}
		ingressPKI, err = loader(item)
		if err != nil {
			return LoadBalancerResult{}, fmt.Errorf("load ingress PKI: %w", err)
		}
		objects = append(objects, ingressTLSSecretObject(ingressPKI))
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return LoadBalancerResult{}, fmt.Errorf("parse kubeconfig for Cilium apply: %w", err)
	}
	return r.reconcile(ctx, item, config, objects, ingressPKI)
}

func (r CiliumReconciler) reconcile(ctx context.Context, item cluster.Cluster, config *rest.Config, objects []unstructured.Unstructured, ingressPKI ingressPKI) (LoadBalancerResult, error) {
	if r.PollInterval <= 0 {
		r.PollInterval = time.Second
	}
	// Cilium is reconciled straight after bootstrap, before the Nodes-Ready
	// wait, and kube-apiserver comes up seconds to minutes later. The
	// discovery-backed applies below treat a refused dial as fatal, so gate
	// them on the API server actually answering.
	if err := waitForAPIServer(ctx, config, r.PollInterval); err != nil {
		return LoadBalancerResult{}, annotateAPIServerTimeout(err, r.MirrorOffline)
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
	if err := applyAllAwaitingDefaultNamespaces(ctx, dynamicClient, mapper, namespaces, r.PollInterval); err != nil {
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
	if err := applyAllAwaitingDefaultNamespaces(ctx, dynamicClient, mapper, chart, r.PollInterval); err != nil {
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
	if err := waitForCiliumCRDs(ctx, dynamicClient, r.PollInterval, item); err != nil {
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
		if err := recreateLegacyCiliumProbeService(ctx, clientset, r.PollInterval); err != nil {
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
	if err := waitForIngressTLS(ctx, item, vip, ingressPKI, r.NewHTTPSProbeTransport); err != nil {
		return LoadBalancerResult{}, err
	}
	return LoadBalancerResult{VIP: vip, Narration: ciliumNarration(item, true)}, nil
}

// apiServerDefaultNamespaces are the namespaces kube-apiserver creates for
// itself. They exist moments after it starts answering, not before it: a
// substrate-only create followed straight by a CNI reconcile can win that race
// on a warm cache and be handed a NotFound for kube-system (#389).
var apiServerDefaultNamespaces = []string{"kube-system", "default", "kube-public", "kube-node-lease"}

// missingDefaultNamespace reports the one NotFound a fresh control plane cures
// on its own: an apply into a default namespace the API server has not created
// yet. Every other NotFound stays fatal — a chart object addressed to a
// namespace nobody will ever create must not burn the provisioning budget.
func missingDefaultNamespace(err error) bool {
	if !apierrors.IsNotFound(err) {
		return false
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		if details := status.Status().Details; details != nil && details.Kind == "namespaces" {
			return slices.Contains(apiServerDefaultNamespaces, details.Name)
		}
	}
	// A wrapped status error can lose its details; the message is then the only
	// evidence of which namespace was missing.
	for _, namespace := range apiServerDefaultNamespaces {
		if strings.Contains(err.Error(), fmt.Sprintf("namespaces %q not found", namespace)) {
			return true
		}
	}
	return false
}

// applyAllAwaitingDefaultNamespaces is applyAll with the API server's own
// startup race retried inside the caller's budget instead of aborting the
// create (#389). Server-side apply is idempotent, so replaying the whole set
// after a transient miss costs a repeated apply and nothing else.
func applyAllAwaitingDefaultNamespaces(
	ctx context.Context,
	client dynamic.Interface,
	mapper meta.RESTMapper,
	objects []unstructured.Unstructured,
	interval time.Duration,
) error {
	return poll(ctx, GateCiliumApply, interval, func(ctx context.Context) error {
		err := applyAll(ctx, client, mapper, objects)
		if err == nil || missingDefaultNamespace(err) {
			return err
		}
		return terminal(err)
	})
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
		resource, live, found, err := getDynamicObject(ctx, client, mapper, candidate)
		if err != nil {
			return fmt.Errorf("get Hubble %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		if !found {
			continue
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
		_, live, found, err := getDynamicObject(ctx, client, mapper, candidate)
		if err != nil {
			return fmt.Errorf("get Hubble %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		if !found {
			continue
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
		resource, live, found, err := getDynamicObject(ctx, client, mapper, candidate)
		if meta.IsNoMatchError(err) {
			// The alternative announcement mode's CRDs were never installed
			// on this cluster, so no stale object of this kind can exist.
			// Only this caller may tolerate an unserved kind: the Hubble and
			// storage paths deal in kinds that must always be mappable.
			resources = append(resources, nil)
			continue
		}
		if err != nil {
			return fmt.Errorf("get stale Cilium %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		if !found {
			resources = append(resources, nil)
			continue
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

func recreateLegacyCiliumProbeService(ctx context.Context, client kubernetes.Interface, interval time.Duration) error {
	service, err := client.CoreV1().Services(probeNamespace).Get(ctx, "lb-probe", metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("get Cilium probe Service: %w", err)
	}
	if service.Spec.Type != "LoadBalancer" {
		return nil
	}
	if service.Labels["talosbox.dev/managed"] != "true" {
		return errors.New("refuse to replace unmanaged Cilium probe Service")
	}
	if err := client.CoreV1().Services(probeNamespace).Delete(ctx, "lb-probe", metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy Cilium probe Service: %w", err)
	}
	return poll(ctx, GateLoadBalancerVIP, interval, func(ctx context.Context) error {
		_, err := client.CoreV1().Services(probeNamespace).Get(ctx, "lb-probe", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return errors.New("legacy Cilium probe Service deletion is still pending")
	})
}

func getDynamicObject(
	ctx context.Context,
	client dynamic.Interface,
	mapper meta.RESTMapper,
	candidate unstructured.Unstructured,
) (dynamic.ResourceInterface, *unstructured.Unstructured, bool, error) {
	mapping, err := mapper.RESTMapping(candidate.GroupVersionKind().GroupKind(), candidate.GroupVersionKind().Version)
	if err != nil {
		return nil, nil, false, fmt.Errorf("map %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
	}
	var resource dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		resource = client.Resource(mapping.Resource).Namespace(candidate.GetNamespace())
	} else {
		resource = client.Resource(mapping.Resource)
	}
	live, err := resource.Get(ctx, candidate.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return resource, nil, false, nil
	}
	if err != nil {
		return resource, nil, false, err
	}
	return resource, live, true, nil
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
	name := shellquote.Quote(item.Name)
	narration := []string{
		"Cilium chart: ≈ tbx manifests " + name + " objects | kubectl apply --server-side -f -",
	}
	if loadBalancer {
		narration = append(narration, "Cilium LoadBalancer extras: ≈ tbx manifests "+name+" extras | kubectl apply --server-side -f -")
	}
	if item.Hubble {
		narration = append(narration, "Hubble UI: ≈ kubectl port-forward -n "+ciliumNamespace+" service/hubble-ui 12000:80 # http://localhost:12000")
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
		{"/apis/apps/v1/namespaces/" + ciliumNamespace + "/deployments/cilium-operator", "deployment"},
		{"/apis/apps/v1/namespaces/" + ciliumNamespace + "/daemonsets/cilium", "daemonset"},
		{"/apis/apps/v1/namespaces/" + ciliumNamespace + "/daemonsets/cilium-envoy", "daemonset"},
	} {
		if err := ciliumWorkloadReady(ctx, transport, server, workload.path, workload.kind); err != nil {
			return err
		}
	}
	if item.LB {
		if err := ciliumDefaultIngressClassState(ctx, transport, server); err != nil {
			return err
		}
		vip, err := ciliumIngressServiceState(ctx, transport, server, item)
		if err != nil {
			return err
		}
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
		if err := ciliumProbeServiceState(ctx, transport, server); err != nil {
			return err
		}
		pki, err := ciliumConvergedIngressPKI(item, time.Now().UTC())
		if err != nil {
			return err
		}
		if err := ciliumProbeIngressState(ctx, transport, server, item); err != nil {
			return err
		}
		if err := ciliumProbeTLSSecretState(ctx, transport, server, pki); err != nil {
			return err
		}
		if err := ciliumDirectHTTPProbe(ctx, item, vip, nil); err != nil {
			return err
		}
		if err := ciliumDirectTLSProbe(ctx, item, vip, pki); err != nil {
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
		if err := ciliumObjectState(ctx, transport, server, ciliumProbeIngressPath(), false); err != nil {
			return err
		}
		if err := ciliumObjectState(ctx, transport, server, ciliumProbeTLSSecretPath(), false); err != nil {
			return err
		}
	}
	return ciliumHubbleConverged(ctx, transport, server, item)
}

func ciliumConvergedIngressPKI(item cluster.Cluster, now time.Time) (ingressPKI, error) {
	pki, err := loadIngressPKIForCluster(item)
	if err != nil {
		return ingressPKI{}, err
	}
	if ingressLeafDueForRenewal(pki.LeafCertificate, now) {
		return ingressPKI{}, errors.New("ingress certificate is due for renewal")
	}
	return pki, nil
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

func ciliumProbeIngressPath() string {
	return "/apis/networking.k8s.io/v1/namespaces/" + probeNamespace + "/ingresses/lb-probe"
}

func ciliumProbeTLSSecretPath() string {
	return "/api/v1/namespaces/" + probeNamespace + "/secrets/" + ingressTLSSecretName
}

func ciliumDefaultIngressClassState(ctx context.Context, transport http.RoundTripper, server string) error {
	response, err := ciliumGet(ctx, transport, server, "/apis/networking.k8s.io/v1/ingressclasses/cilium")
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("cilium default ingressclass: %s", response.Status)
	}
	var object struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(response.Body).Decode(&object); err != nil {
		return fmt.Errorf("decode cilium ingressclass: %w", err)
	}
	if object.Metadata.Annotations["ingressclass.kubernetes.io/is-default-class"] != "true" {
		return errors.New("cilium ingressclass is not the default")
	}
	return nil
}

func ciliumIngressServiceState(ctx context.Context, transport http.RoundTripper, server string, item cluster.Cluster) (string, error) {
	response, err := ciliumGet(ctx, transport, server, "/api/v1/namespaces/"+ciliumNamespace+"/services/cilium-ingress")
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cilium ingress service: %s", response.Status)
	}
	var service struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Status struct {
			LoadBalancer struct {
				Ingress []struct {
					IP string `json:"ip"`
				} `json:"ingress"`
			} `json:"loadBalancer"`
		} `json:"status"`
	}
	if err := json.NewDecoder(response.Body).Decode(&service); err != nil {
		return "", fmt.Errorf("decode cilium ingress service: %w", err)
	}
	want := fmt.Sprintf("172.30.%d.200", item.SubnetIndex)
	if service.Metadata.Annotations["lbipam.cilium.io/ips"] != want {
		return "", fmt.Errorf("cilium ingress service annotation = %q, want %s", service.Metadata.Annotations["lbipam.cilium.io/ips"], want)
	}
	if len(service.Status.LoadBalancer.Ingress) != 1 || service.Status.LoadBalancer.Ingress[0].IP != want {
		return "", fmt.Errorf("cilium ingress service VIP = %v, want %s", service.Status.LoadBalancer.Ingress, want)
	}
	return want, nil
}

func ciliumProbeServiceState(ctx context.Context, transport http.RoundTripper, server string) error {
	response, err := ciliumGet(ctx, transport, server, ciliumProbeServicePath())
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("cilium desired object %s: %s", ciliumProbeServicePath(), response.Status)
	}
	var object struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Type string `json:"type"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(response.Body).Decode(&object); err != nil {
		return fmt.Errorf("decode Cilium object %s: %w", ciliumProbeServicePath(), err)
	}
	if object.Metadata.Labels["talosbox.dev/managed"] != "true" {
		return errors.New("cilium probe Service is not owned by talosbox")
	}
	if object.Spec.Type != "ClusterIP" {
		return fmt.Errorf("cilium probe Service type = %s, want ClusterIP", object.Spec.Type)
	}
	return nil
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
	object, err := ciliumDesiredObject(ctx, transport, server, path)
	if err != nil {
		return err
	}
	if object.GetAnnotations()[key] != value && object.GetLabels()[key] != value {
		return fmt.Errorf("cilium desired object %s is not owned by talosbox", path)
	}
	return nil
}

// ciliumAnnouncementState additionally checks the ownership annotation. The
// resource names are stable, but ownership prevents a coincidental attendee
// resource from being treated as talosbox's declarative announcement set.
func ciliumAnnouncementState(ctx context.Context, transport http.RoundTripper, server, path string) error {
	object, err := ciliumDesiredObject(ctx, transport, server, path)
	if err != nil {
		return err
	}
	if object.GetAnnotations()[announcementOwnershipAnnotation] != fieldManager {
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

func ciliumDesiredObject(ctx context.Context, transport http.RoundTripper, server, path string) (unstructured.Unstructured, error) {
	response, err := ciliumGet(ctx, transport, server, path)
	if err != nil {
		return unstructured.Unstructured{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return unstructured.Unstructured{}, fmt.Errorf("cilium desired object %s: %s", path, response.Status)
	}
	object := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&object); err != nil {
		return unstructured.Unstructured{}, fmt.Errorf("decode Cilium object %s: %w", path, err)
	}
	return unstructured.Unstructured{Object: object}, nil
}

func ciliumProbeIngressState(ctx context.Context, transport http.RoundTripper, server string, item cluster.Cluster) error {
	object, err := ciliumDesiredObject(ctx, transport, server, ciliumProbeIngressPath())
	if err != nil {
		return err
	}
	if object.GetLabels()["talosbox.dev/managed"] != "true" {
		return fmt.Errorf("cilium desired object %s is not owned by talosbox", ciliumProbeIngressPath())
	}
	wildcard := "*." + item.EffectiveDomain()
	if className, _, _ := unstructured.NestedString(object.Object, "spec", "ingressClassName"); className != "cilium" {
		return fmt.Errorf("cilium probe Ingress class = %q, want cilium", className)
	}
	if !ciliumProbeIngressMatches(object.Object, wildcard) {
		return fmt.Errorf("cilium probe Ingress does not match wildcard route %s -> lb-probe:80", wildcard)
	}
	return nil
}

func ciliumProbeIngressMatches(object map[string]any, wildcard string) bool {
	tls, found, err := unstructured.NestedSlice(object, "spec", "tls")
	if err != nil || !found || len(tls) != 1 {
		return false
	}
	tlsEntry, ok := tls[0].(map[string]any)
	if !ok {
		return false
	}
	tlsHosts, found, err := unstructured.NestedStringSlice(tlsEntry, "hosts")
	if err != nil || !found || len(tlsHosts) != 1 || tlsHosts[0] != wildcard {
		return false
	}
	secretName, _, _ := unstructured.NestedString(tlsEntry, "secretName")
	if secretName != ingressTLSSecretName {
		return false
	}
	rules, found, err := unstructured.NestedSlice(object, "spec", "rules")
	if err != nil || !found || len(rules) != 1 {
		return false
	}
	rule, ok := rules[0].(map[string]any)
	if !ok {
		return false
	}
	host, _, _ := unstructured.NestedString(rule, "host")
	if host != wildcard {
		return false
	}
	paths, found, err := unstructured.NestedSlice(rule, "http", "paths")
	if err != nil || !found || len(paths) != 1 {
		return false
	}
	path, ok := paths[0].(map[string]any)
	if !ok {
		return false
	}
	actualPath, _, _ := unstructured.NestedString(path, "path")
	pathType, _, _ := unstructured.NestedString(path, "pathType")
	serviceName, _, _ := unstructured.NestedString(path, "backend", "service", "name")
	port, ok := ciliumProbePortNumber(path)
	return ok && actualPath == "/" && pathType == "Prefix" && serviceName == "lb-probe" && port == 80
}

func ciliumProbeTLSSecretState(ctx context.Context, transport http.RoundTripper, server string, pki ingressPKI) error {
	object, err := ciliumDesiredObject(ctx, transport, server, ciliumProbeTLSSecretPath())
	if err != nil {
		return err
	}
	if object.GetLabels()["talosbox.dev/managed"] != "true" {
		return fmt.Errorf("cilium desired object %s is not owned by talosbox", ciliumProbeTLSSecretPath())
	}
	return ciliumProbeTLSSecretMatches(object.Object, pki)
}

func ciliumProbeTLSSecretMatches(object map[string]any, pki ingressPKI) error {
	return ingressTLSSecretExactObjectMatch(object, pki)
}

func ciliumProbePortNumber(path map[string]any) (int64, bool) {
	value, found, err := unstructured.NestedFieldNoCopy(path, "backend", "service", "port", "number")
	if err != nil || !found {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case float32:
		return int64(typed), float32(int64(typed)) == typed
	case float64:
		return int64(typed), float64(int64(typed)) == typed
	default:
		return 0, false
	}
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
spec:
  type: ClusterIP
  selector:
    app: talosbox-lb-probe
  ports:
    - port: 80
      targetPort: 8080
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: lb-probe
  namespace: %s
  labels:
    talosbox.dev/managed: "true"
spec:
  ingressClassName: cilium
  tls:
    - hosts:
        - "*.%s"
      secretName: ingress-wildcard-tls
  rules:
    - host: "*.%s"
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: lb-probe
                port:
                  number: 80
`, probeNamespace, probeNamespace, probeNamespace, probeNamespace, item.EffectiveDomain(), item.EffectiveDomain())
}

var ciliumDirectHTTPProbe = func(ctx context.Context, item cluster.Cluster, vip string, httpClient *http.Client) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+vip+"/", nil)
	if err != nil {
		return err
	}
	setProbeHost(request, item)
	response, err := vipHTTPClient(httpClient).Do(request)
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
}

var ciliumDirectTLSProbe = func(ctx context.Context, item cluster.Cluster, vip string, pki ingressPKI) error {
	return waitForIngressTLS(ctx, item, vip, pki, nil)
}

func waitForIngressTLS(
	ctx context.Context,
	item cluster.Cluster,
	vip string,
	pki ingressPKI,
	newTransport func(*tls.Config, func(context.Context, string, string) (net.Conn, error)) http.RoundTripper,
) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pki.CACertPEM) {
		return errors.New("load ingress CA for HTTPS readiness")
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: "probe." + item.EffectiveDomain(),
	}
	client := &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: newIngressHTTPSProbeTransport(tlsConfig, func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, net.JoinHostPort(vip, "443"))
		}, newTransport),
	}
	return poll(ctx, GateIngressTLS, defaultPollInterval, func(ctx context.Context) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+vip+"/", nil)
		if err != nil {
			return terminal(err)
		}
		setProbeHost(request, item)
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		if err := response.Body.Close(); err != nil {
			return fmt.Errorf("close LoadBalancer TLS probe response: %w", err)
		}
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("LoadBalancer TLS probe response = %s", response.Status)
		}
		return nil
	})
}

func newIngressHTTPSProbeTransport(
	tlsConfig *tls.Config,
	dialContext func(context.Context, string, string) (net.Conn, error),
	override func(*tls.Config, func(context.Context, string, string) (net.Conn, error)) http.RoundTripper,
) http.RoundTripper {
	if override != nil {
		return override(tlsConfig, dialContext)
	}
	transport := defaultProxylessTransport()
	transport.TLSClientConfig = tlsConfig
	transport.DialContext = dialContext
	return transport
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
		case "Deployment", "Service", "Ingress", "Secret":
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

// waitForAPIServer polls /version until kube-apiserver answers, so the error
// on expiry names the endpoint that never came up rather than whichever apply
// happened to dial first. Only bootstrap-transient failures (refused dials,
// resets, timeouts, 5xx) are retried: bad credentials or a broken trust chain
// cannot heal by waiting and must fail as fast as they did before this gate.
func waitForAPIServer(ctx context.Context, config *rest.Config, interval time.Duration) error {
	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes discovery client: %w", err)
	}
	if err := poll(ctx, GateAPIServer, interval, func(ctx context.Context) error {
		_, err := client.RESTClient().Get().AbsPath("/version").DoRaw(ctx)
		if err == nil {
			return nil
		}
		if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) || isUntrustedCertificateError(err) {
			return terminal(err)
		}
		return err
	}); err != nil {
		return fmt.Errorf("wait for kube-apiserver at %s: %w", config.Host, err)
	}
	return nil
}

// annotateAPIServerTimeout names the failure a deadline on the very first
// API-server wait almost always hides when the mirror is offline: the CRI pod
// sandbox image was never cached, so every static pod loops on an offline miss
// from the mirror and the control plane never comes up at all. The client this code
// holds here talks to kube-apiserver, which by definition is not answering, so
// no CRI or kubelet state is cheaply readable at this point — this is a
// pointer at the check that settles it, not a verdict, and it is only added
// when the provisioner knows the mirror is serving from cache alone.
func annotateAPIServerTimeout(err error, mirrorOffline bool) error {
	if err == nil || !mirrorOffline || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w; the mirror is offline, so no node can pull an image it never cached: "+
		"verify the CRI pod sandbox image %s is present with `tbx cache warm --check --deep`, "+
		"and re-run `tbx cache pull` online if it is missing", err, KubernetesSandboxImage)
}

// isUntrustedCertificateError matches only trust-chain failures that cannot
// heal by waiting: the kubeconfig pins the CA, so an unknown authority or a
// structurally unusable chain is permanent. Expired/not-yet-valid certs
// (guest clock skew right after boot) and hostname mismatches (serving-cert
// SANs lagging a fresh control-plane lease) resolve on their own and must
// stay retryable.
func isUntrustedCertificateError(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		// NotAuthorizedToSign never surfaces here: Go's chain builder folds
		// it into UnknownAuthorityError, which the branch above catches.
		switch invalid.Reason {
		case x509.IncompatibleUsage, x509.CANotAuthorizedForThisName:
			return true
		}
	}
	return false
}

func waitForCilium(ctx context.Context, client kubernetes.Interface, interval time.Duration) error {
	return poll(ctx, GateCilium, interval, func(ctx context.Context) error {
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
	return poll(ctx, GateHubble, interval, func(ctx context.Context) error {
		for _, name := range []string{"hubble-relay", "hubble-ui"} {
			deployment, err := client.AppsV1().Deployments(ciliumNamespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil || !deploymentReady(deployment) {
				return fmt.Errorf("hubble %s is not ready", name)
			}
		}
		return nil
	})
}

// waitForCiliumCRDs waits only for CRDs the rendered chart actually creates.
// The operator registers a feature's CRDs only when the installed values
// enable it, so the wait set must mirror manifests.CiliumValues: LB-IPAM is
// always on, l2announcements only when the LB path is L2 (lb && !bgp), and
// the BGP trio only when bgpControlPlane is enabled. Waiting for anything
// else deadlines on a CRD that never appears (#295).
func waitForCiliumCRDs(ctx context.Context, client dynamic.Interface, interval time.Duration, item cluster.Cluster) error {
	names := []string{"ciliumloadbalancerippools.cilium.io"}
	if item.LB && !item.BGP {
		names = append(names, "ciliuml2announcementpolicies.cilium.io")
	}
	if item.BGP {
		names = append(names,
			"ciliumbgpclusterconfigs.cilium.io",
			"ciliumbgppeerconfigs.cilium.io",
			"ciliumbgpadvertisements.cilium.io",
		)
	}
	resource := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	return poll(ctx, GateCiliumCRDs, interval, func(ctx context.Context) error {
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
