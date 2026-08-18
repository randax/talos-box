package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"go.yaml.in/yaml/v4"
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
	longhornChartVersion       = "1.12.0"
	longhornChartSHA256        = "869bb20701b154473606f1e8967b27f34f2448a2dfe6eb8970f1cae6957384f5"
	longhornNamespace          = "longhorn-system"
	longhornStorageClass       = "longhorn"
	longhornProvisioner        = "driver.longhorn.io"
	longhornManagerName        = "longhorn-manager"
	longhornDriverDeployerName = "longhorn-driver-deployer"
	longhornUIName             = "longhorn-ui"
)

//go:embed assets/longhorn-1.12.0.tgz
var longhornChart []byte

var longhornNodeResource = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "nodes"}

// LonghornReconciler installs and verifies the pinned Longhorn chart through
// the same host-side Helm render and server-side apply path used by the rest of
// provisioning.
type LonghornReconciler struct {
	PollInterval time.Duration
}

func (r LonghornReconciler) Reconcile(ctx context.Context, item cluster.Cluster, kubeconfig []byte) (StorageResult, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return StorageResult{}, fmt.Errorf("parse kubeconfig for Kubernetes apply: %w", err)
	}
	return r.reconcile(ctx, config, item)
}

// ProbeLonghornStorage re-runs the shared default-StorageClass write/readback
// probe without reinstalling Longhorn, keeping storage-live status tied to the
// actual PVC data path after daemon restarts.
func ProbeLonghornStorage(ctx context.Context, clusterName string, kubeconfig []byte, interval time.Duration) (StorageProbeOutcome, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return StorageProbeOutcome{}, fmt.Errorf("parse kubeconfig for storage probe: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return StorageProbeOutcome{}, fmt.Errorf("create Kubernetes discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return StorageProbeOutcome{}, fmt.Errorf("create Kubernetes apply client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return StorageProbeOutcome{}, fmt.Errorf("create Kubernetes readiness client: %w", err)
	}
	if interval <= 0 {
		interval = time.Second
	}
	return runStorageProbe(ctx, dynamicClient, mapper, clientset, storageProbeSpec{
		ExpectedStorageClass: longhornStorageClass,
		ProbeImage:           localPathHelperImage,
		Engine:               cluster.CSILonghorn,
		ClusterName:          clusterName,
	}, interval)
}

func (r LonghornReconciler) reconcile(ctx context.Context, config *rest.Config, item cluster.Cluster) (StorageResult, error) {
	if r.PollInterval <= 0 {
		r.PollInterval = time.Second
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return StorageResult{}, fmt.Errorf("create Kubernetes discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return StorageResult{}, fmt.Errorf("create Kubernetes apply client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return StorageResult{}, fmt.Errorf("create Kubernetes readiness client: %w", err)
	}
	objects, err := renderLonghornForPlacement(ctx, dynamicClient, item)
	if err != nil {
		return StorageResult{}, err
	}

	namespaces, chartObjects, crds := partitionLonghornObjects(objects)
	if err := applyAll(ctx, dynamicClient, mapper, namespaces); err != nil {
		return StorageResult{}, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, crds); err != nil {
		return StorageResult{}, err
	}
	if err := waitForCRDs(ctx, dynamicClient, mapper, crds, "Longhorn", r.PollInterval); err != nil {
		return StorageResult{}, err
	}
	mapper.Reset()
	if err := replaceDriftedStorageClass(ctx, dynamicClient, mapper, chartObjects, r.PollInterval); err != nil {
		return StorageResult{}, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, chartObjects); err != nil {
		return StorageResult{}, err
	}
	if err := waitForLonghorn(ctx, clientset, r.PollInterval); err != nil {
		return StorageResult{}, err
	}
	if err := reconcileLonghornControlPlaneScheduling(ctx, dynamicClient, item, r.PollInterval); err != nil {
		return StorageResult{}, err
	}
	outcome, err := runStorageProbe(ctx, dynamicClient, mapper, clientset, storageProbeSpec{
		ExpectedStorageClass: longhornStorageClass,
		ProbeImage:           localPathHelperImage,
		Engine:               cluster.CSILonghorn,
		ClusterName:          item.Name,
	}, r.PollInterval)
	if err != nil {
		return StorageResult{}, err
	}
	return storageResultFromProbe(outcome, []string{
		"≈ helm template longhorn longhorn/longhorn --version " + longhornChartVersion + " -n " + longhornNamespace + " | kubectl apply --server-side -f -",
		"≈ kubectl apply --server-side -f - # storage probe PVC + writer/reader pods",
	}), nil
}

// renderLonghorn renders the chart for the cluster's declared topology: a
// cluster with workers keeps every Longhorn component off its control planes
// (see longhornValues), a worker-less one has nowhere else to run them.
func renderLonghorn(item cluster.Cluster) ([]unstructured.Unstructured, error) {
	return renderLonghornWithTolerations(item, !clusterHasWorkers(item))
}

// renderLonghornForPlacement renders the chart for what the live cluster holds
// rather than for its declared shape alone. A cluster that ran worker-less may
// still hold replicas on a control plane; the components that serve them have
// to keep tolerating the taint a joining worker returned, even though a
// cluster that never ran worker-less must keep them off (#339).
func renderLonghornForPlacement(ctx context.Context, client dynamic.Interface, item cluster.Cluster) ([]unstructured.Unstructured, error) {
	tolerate := !clusterHasWorkers(item)
	if !tolerate {
		holdsReplicas, err := controlPlaneHoldsLonghornReplicas(ctx, client, item)
		if err != nil {
			return nil, err
		}
		tolerate = holdsReplicas
	}
	return renderLonghornWithTolerations(item, tolerate)
}

// controlPlaneHoldsLonghornReplicas reports whether any control plane still
// holds a replica. Longhorn's CRDs are absent before the first install, and an
// absent CRD means no replica anywhere — not a failure to propagate.
func controlPlaneHoldsLonghornReplicas(ctx context.Context, client dynamic.Interface, item cluster.Cluster) (bool, error) {
	replicas, err := client.Resource(longhornReplicaResource).Namespace(longhornNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("list longhorn replicas: %w", err)
	}
	controlPlanes := make(map[string]bool)
	for _, node := range item.Nodes {
		if node.Role == cluster.RoleControlPlane {
			controlPlanes[node.Name] = true
		}
	}
	for i := range replicas.Items {
		if controlPlanes[nestedString(&replicas.Items[i], "spec", "nodeID")] {
			return true, nil
		}
	}
	return false, nil
}

func renderLonghornWithTolerations(item cluster.Cluster, tolerateControlPlane bool) ([]unstructured.Unstructured, error) {
	if item.CSI != cluster.CSILonghorn {
		return nil, errors.New("longhorn rendering requires csi: longhorn")
	}
	if actual := fmt.Sprintf("%x", sha256.Sum256(longhornChart)); actual != longhornChartSHA256 {
		return nil, fmt.Errorf("embedded Longhorn chart checksum = %s, want %s", actual, longhornChartSHA256)
	}
	chart, err := loader.LoadArchive(bytes.NewReader(longhornChart))
	if err != nil {
		return nil, fmt.Errorf("load embedded Longhorn chart: %w", err)
	}
	if chart.Metadata.Version != longhornChartVersion {
		return nil, fmt.Errorf("embedded Longhorn chart version = %s, want %s", chart.Metadata.Version, longhornChartVersion)
	}
	storageNodes := storageNodeCount(item)
	replicas := longhornReplicaCount(storageNodes)
	values, err := chartutil.ReadValues([]byte(longhornValues(replicas, storageNodes, tolerateControlPlane)))
	if err != nil {
		return nil, fmt.Errorf("decode Longhorn values: %w", err)
	}
	renderValues, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{Name: "longhorn", Namespace: longhornNamespace}, chartutil.DefaultCapabilities)
	if err != nil {
		return nil, fmt.Errorf("prepare Longhorn render values: %w", err)
	}
	rendered, err := (engine.Engine{}).Render(chart, renderValues)
	if err != nil {
		return nil, fmt.Errorf("render embedded Longhorn chart: %w", err)
	}
	delete(rendered, "longhorn/templates/NOTES.txt")
	_, sorted, err := releaseutil.SortManifests(rendered, chartutil.DefaultVersionSet, releaseutil.InstallOrder)
	if err != nil {
		return nil, fmt.Errorf("sort rendered Longhorn chart: %w", err)
	}

	result := []unstructured.Unstructured{{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"name": longhornNamespace,
				"labels": map[string]any{
					"talosbox.dev/managed":               "true",
					"pod-security.kubernetes.io/enforce": "privileged",
					"pod-security.kubernetes.io/audit":   "privileged",
					"pod-security.kubernetes.io/warn":    "privileged",
				},
			},
		},
	}}
	for _, manifest := range sorted {
		objects, err := decodeObjects([]byte(manifest.Content))
		if err != nil {
			return nil, fmt.Errorf("decode rendered Longhorn %s: %w", manifest.Name, err)
		}
		for i := range objects {
			object := &objects[i]
			ensureManagedLabel(object)
			if object.GetKind() == "ConfigMap" && object.GetName() == "longhorn-storageclass" && object.GetNamespace() == longhornNamespace {
				storageClassObjects, err := decodeLonghornStorageClass(object)
				if err != nil {
					return nil, err
				}
				result = append(result, *object)
				result = append(result, storageClassObjects...)
				continue
			}
			result = append(result, *object)
		}
	}
	return result, nil
}

// storageNodeCount counts the nodes that can host Longhorn replicas: workers,
// or every node on a worker-less cluster, whose control planes tbx makes
// schedulable. Counting reserved control planes would pin the replica target
// above what can ever schedule and leave volumes permanently degraded;
// reconcileLonghornControlPlaneScheduling keeps the live cluster to the same
// set of replica-eligible nodes.
func storageNodeCount(item cluster.Cluster) int {
	workers := 0
	for _, node := range item.Nodes {
		if node.Role == cluster.RoleWorker {
			workers++
		}
	}
	if workers == 0 {
		return len(item.Nodes)
	}
	return workers
}

func longhornReplicaCount(nodes int) int {
	switch {
	case nodes <= 1:
		return 1
	case nodes == 2:
		return 2
	default:
		return 3
	}
}

// longhornValues sizes the stack for the cluster it is going onto. The CSI
// sidecar Deployments default to three replicas each: on a default 2 GiB
// control plane, whose allocatable is already mostly etcd, apiserver and CNI,
// the extra copies plus longhorn-manager churn on the guest OOM killer for as
// long as the cluster lives (#339). They follow the replica-eligible node
// count instead — the same set storageNodeCount derives.
//
// tolerateControlPlane decides whether any Longhorn component may run beside
// etcd. A worker-less cluster has nowhere else to run them. A cluster with
// workers keeps them out, which is what stops the OOM churn: tbx leaves the
// NoSchedule taint on those control planes, so withholding the toleration —
// from the DaemonSet/Deployment pod specs (global.tolerations) and from the
// system-managed components alike (defaultSettings.taintToleration) — pins
// manager, driver deployer, UI, CSI sidecars and instance managers to workers.
// The one exception is a cluster that ran worker-less and still holds replicas
// on a control plane: renderLonghornForPlacement keeps the toleration for it,
// because otherwise neither longhorn-manager nor its instance managers could
// be recreated there and those replicas would fault on the next restart.
// Running there is not the same as being replica-eligible:
// reconcileLonghornControlPlaneScheduling clears spec.allowScheduling on the
// control planes of a cluster that has workers, so tolerating the taint never
// widens replica placement.
func longhornValues(replicas, storageNodes int, tolerateControlPlane bool) string {
	tolerations := "  tolerations: []\n"
	taintToleration := ""
	if tolerateControlPlane {
		tolerations = fmt.Sprintf(`  tolerations:
    - key: %s
      operator: Exists
      effect: NoSchedule
`, controlPlaneTaint)
		taintToleration = fmt.Sprintf("  taintToleration: %s:NoSchedule\n", controlPlaneTaint)
	}
	return fmt.Sprintf(`global:
%[3]spersistence:
  defaultClass: true
  defaultClassReplicaCount: %[1]d
longhornUI:
  replicas: 1
csi:
  attacherReplicaCount: %[2]d
  provisionerReplicaCount: %[2]d
  resizerReplicaCount: %[2]d
  snapshotterReplicaCount: %[2]d
defaultSettings:
  defaultDataPath: /var/lib/longhorn
  defaultReplicaCount: "%[1]d"
%[4]s`, replicas, longhornSidecarReplicaCount(storageNodes), tolerations, taintToleration)
}

// longhornSidecarReplicaCount keeps the CSI sidecars from asking for more
// copies than there are nodes to spread them over, and never drops below one.
func longhornSidecarReplicaCount(storageNodes int) int {
	if storageNodes < 1 {
		return 1
	}
	if storageNodes > 3 {
		return 3
	}
	return storageNodes
}

// reconcileLonghornControlPlaneScheduling holds the live cluster to the
// replica-eligible set storageNodeCount derives: a control plane attracts
// replicas only while the cluster is worker-less. Longhorn schedules onto
// every node its manager runs on, and the manager tolerates the control-plane
// taint (see longhornValues), so the cluster shape has to be expressed on the
// node resource instead. Clearing spec.allowScheduling stops new replicas
// without evicting copies a worker-less phase already placed there — replica
// I/O beside etcd is what the taint exists to prevent.
// LonghornSchedulingConverged is the matching observed-state probe: it gates
// the fast no-op so an interrupted pass rewrites this on the next tbx up.
func reconcileLonghornControlPlaneScheduling(ctx context.Context, client dynamic.Interface, item cluster.Cluster, interval time.Duration) error {
	allowScheduling := !clusterHasWorkers(item)
	for _, node := range item.Nodes {
		if node.Role != cluster.RoleControlPlane {
			continue
		}
		name := node.Name
		if err := poll(ctx, interval, func(ctx context.Context) error {
			return setLonghornNodeScheduling(ctx, client, name, allowScheduling)
		}); err != nil {
			return err
		}
	}
	return nil
}

// LonghornSchedulingConverged observes the Longhorn half of the zero-worker
// invariant: a control plane carries spec.allowScheduling only while the
// cluster is worker-less. ControlPlaneSchedulingConverged covers only the
// Talos half (taint and exclusion label), and longhorn-manager tolerates that
// taint, so without this probe a mutation pass interrupted before its storage
// stage would leave replica placement drift no end-state check can see and no
// rerun of tbx up can repair.
func LonghornSchedulingConverged(ctx context.Context, kubeconfig []byte, item cluster.Cluster) error {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("parse kubeconfig for longhorn scheduling probe: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes dynamic client for longhorn scheduling probe: %w", err)
	}
	return longhornSchedulingConverged(ctx, dynamicClient, item)
}

func longhornSchedulingConverged(ctx context.Context, client dynamic.Interface, item cluster.Cluster) error {
	allowScheduling := !clusterHasWorkers(item)
	nodes := client.Resource(longhornNodeResource).Namespace(longhornNamespace)
	for _, node := range item.Nodes {
		if node.Role != cluster.RoleControlPlane {
			continue
		}
		// A missing node resource is drift, not an absence to skip: the
		// manager DaemonSet registers every node it runs on, so convergence
		// cannot be claimed while a control plane has no Longhorn node.
		live, err := nodes.Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get longhorn node %q: %w", node.Name, err)
		}
		current, err := longhornNodeAllowsScheduling(live, node.Name)
		if err != nil {
			return err
		}
		if current != allowScheduling {
			return fmt.Errorf("longhorn node %q allowScheduling = %t, want %t", node.Name, current, allowScheduling)
		}
	}
	return nil
}

// longhornNodeAllowsScheduling reads spec.allowScheduling. An absent field
// counts as true: Longhorn schedules onto a newly registered node by default.
func longhornNodeAllowsScheduling(live *unstructured.Unstructured, name string) (bool, error) {
	current, found, err := unstructured.NestedBool(live.Object, "spec", "allowScheduling")
	if err != nil {
		return false, fmt.Errorf("decode longhorn node %q scheduling: %w", name, err)
	}
	if !found {
		return true, nil
	}
	return current, nil
}

func setLonghornNodeScheduling(ctx context.Context, client dynamic.Interface, name string, allowScheduling bool) error {
	nodes := client.Resource(longhornNodeResource).Namespace(longhornNamespace)
	// longhorn-manager creates the node resource once its pod registers the
	// node; the manager DaemonSet is Ready by this point, so a missing
	// resource is a race worth retrying rather than a failure.
	live, err := nodes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get longhorn node %q: %w", name, err)
	}
	current, err := longhornNodeAllowsScheduling(live, name)
	if err != nil {
		return terminal(err)
	}
	if current == allowScheduling {
		return nil
	}
	if err := unstructured.SetNestedField(live.Object, allowScheduling, "spec", "allowScheduling"); err != nil {
		return terminal(fmt.Errorf("set longhorn node %q scheduling: %w", name, err))
	}
	if _, err := nodes.Update(ctx, live, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update longhorn node %q scheduling: %w", name, err)
	}
	return nil
}

// decodeLonghornStorageClass extracts the StorageClass definitions Longhorn
// keeps in the longhorn-storageclass ConfigMap so tbx can apply them itself,
// labeled and annotated as the cluster default. The definitions are written
// back into the ConfigMap: longhorn-driver-deployer re-creates the class from
// that yaml verbatim on every start, so a definition without the managed label
// resurfaces as a user-owned StorageClass and the removal guard then refuses
// every switch away from longhorn (#338).
func decodeLonghornStorageClass(configMap *unstructured.Unstructured) ([]unstructured.Unstructured, error) {
	data, found, err := unstructured.NestedStringMap(configMap.Object, "data")
	if err != nil || !found {
		return nil, fmt.Errorf("longhorn storage class config map data missing: found=%v err=%v", found, err)
	}
	rendered, ok := data["storageclass.yaml"]
	if !ok || strings.TrimSpace(rendered) == "" {
		return nil, errors.New(`longhorn storage class config map is missing "storageclass.yaml"`)
	}
	objects, err := decodeObjects([]byte(rendered))
	if err != nil {
		return nil, fmt.Errorf("decode Longhorn storage class config map: %w", err)
	}
	for i := range objects {
		object := &objects[i]
		ensureManagedLabel(object)
		if object.GetKind() == "StorageClass" && object.GetName() == longhornStorageClass {
			ensureAnnotation(object, "storageclass.kubernetes.io/is-default-class", "true")
			ensureAnnotation(object, "storageclass.beta.kubernetes.io/is-default-class", "true")
		}
	}
	if err := setLonghornStorageClassDefinition(configMap, objects); err != nil {
		return nil, err
	}
	return objects, nil
}

// setLonghornStorageClassDefinition re-encodes the labeled StorageClass
// definitions into the ConfigMap Longhorn reads them from.
func setLonghornStorageClassDefinition(configMap *unstructured.Unstructured, objects []unstructured.Unstructured) error {
	documents := make([]string, 0, len(objects))
	for i := range objects {
		encoded, err := yaml.Marshal(objects[i].Object)
		if err != nil {
			return fmt.Errorf("encode Longhorn storage class definition: %w", err)
		}
		documents = append(documents, string(encoded))
	}
	if err := unstructured.SetNestedField(configMap.Object, strings.Join(documents, "---\n"), "data", "storageclass.yaml"); err != nil {
		return fmt.Errorf("set Longhorn storage class definition: %w", err)
	}
	return nil
}

func partitionLonghornObjects(objects []unstructured.Unstructured) (namespaces, chart, crds []unstructured.Unstructured) {
	for _, object := range objects {
		switch object.GetKind() {
		case "Namespace":
			namespaces = append(namespaces, object)
		case "CustomResourceDefinition":
			crds = append(crds, object)
		default:
			chart = append(chart, object)
		}
	}
	return namespaces, chart, crds
}

func waitForLonghorn(ctx context.Context, client kubernetes.Interface, interval time.Duration) error {
	return poll(ctx, interval, func(ctx context.Context) error {
		manager, err := client.AppsV1().DaemonSets(longhornNamespace).Get(ctx, longhornManagerName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		driver, err := client.AppsV1().Deployments(longhornNamespace).Get(ctx, longhornDriverDeployerName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		ui, err := client.AppsV1().Deployments(longhornNamespace).Get(ctx, longhornUIName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !daemonSetReady(manager) || !deploymentReady(driver) || !deploymentReady(ui) {
			return errors.New("longhorn manager, driver deployer, or ui is not Ready")
		}
		return nil
	})
}
