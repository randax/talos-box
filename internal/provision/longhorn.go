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
	longhornChartVersion       = "1.12.0"
	longhornChartSHA256        = "869bb20701b154473606f1e8967b27f34f2448a2dfe6eb8970f1cae6957384f5"
	longhornNamespace          = "longhorn-system"
	longhornStorageClass       = "longhorn"
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
	objects, err := renderLonghorn(item)
	if err != nil {
		return StorageResult{}, err
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return StorageResult{}, fmt.Errorf("parse kubeconfig for Kubernetes apply: %w", err)
	}
	return r.reconcile(ctx, config, item, objects)
}

// ProbeLonghornStorage re-runs the shared default-StorageClass write/readback
// probe without reinstalling Longhorn, keeping storage-live status tied to the
// actual PVC data path after daemon restarts.
func ProbeLonghornStorage(ctx context.Context, kubeconfig []byte, interval time.Duration) error {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("parse kubeconfig for storage probe: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes apply client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes readiness client: %w", err)
	}
	if interval <= 0 {
		interval = time.Second
	}
	return runStorageProbe(ctx, dynamicClient, mapper, clientset, storageProbeSpec{
		ExpectedStorageClass: longhornStorageClass,
		ProbeImage:           localPathHelperImage,
	}, interval)
}

func (r LonghornReconciler) reconcile(ctx context.Context, config *rest.Config, item cluster.Cluster, objects []unstructured.Unstructured) (StorageResult, error) {
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
	if err := runStorageProbe(ctx, dynamicClient, mapper, clientset, storageProbeSpec{
		ExpectedStorageClass: longhornStorageClass,
		ProbeImage:           localPathHelperImage,
	}, r.PollInterval); err != nil {
		return StorageResult{}, err
	}
	return StorageResult{Narration: []string{
		"≈ helm template longhorn longhorn/longhorn --version " + longhornChartVersion + " -n " + longhornNamespace + " | kubectl apply --server-side -f -",
		"≈ kubectl apply --server-side -f - # storage probe PVC + writer/reader pods",
	}, Phase: StoragePhaseLive, Live: true}, nil
}

func renderLonghorn(item cluster.Cluster) ([]unstructured.Unstructured, error) {
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
	replicas := longhornReplicaCount(storageNodeCount(item))
	values, err := chartutil.ReadValues([]byte(longhornValues(replicas)))
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

// longhornValues tolerates controlPlaneTaint in both the DaemonSet/Deployment
// pod specs (global.tolerations) and the system-managed components
// (defaultSettings.taintToleration). Crossing the zero-worker boundary by
// mutation returns that taint to a control plane that still holds replicas
// written while the cluster was worker-less; without a toleration neither
// longhorn-manager nor its instance managers could ever be recreated there and
// those replicas would fault on the next restart. Running there is not the
// same as being replica-eligible: reconcileLonghornControlPlaneScheduling
// clears spec.allowScheduling on the control planes of a cluster that has
// workers, so tolerating the taint never widens replica placement.
func longhornValues(replicas int) string {
	return fmt.Sprintf(`global:
  tolerations:
    - key: %[1]s
      operator: Exists
      effect: NoSchedule
persistence:
  defaultClass: true
  defaultClassReplicaCount: %[2]d
defaultSettings:
  defaultDataPath: /var/lib/longhorn
  defaultReplicaCount: "%[2]d"
  taintToleration: %[1]s:NoSchedule
`, controlPlaneTaint, replicas)
}

// reconcileLonghornControlPlaneScheduling holds the live cluster to the
// replica-eligible set storageNodeCount derives: a control plane attracts
// replicas only while the cluster is worker-less. Longhorn schedules onto
// every node its manager runs on, and the manager tolerates the control-plane
// taint (see longhornValues), so the cluster shape has to be expressed on the
// node resource instead. Clearing spec.allowScheduling stops new replicas
// without evicting copies a worker-less phase already placed there — replica
// I/O beside etcd is what the taint exists to prevent.
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

func setLonghornNodeScheduling(ctx context.Context, client dynamic.Interface, name string, allowScheduling bool) error {
	nodes := client.Resource(longhornNodeResource).Namespace(longhornNamespace)
	// longhorn-manager creates the node resource once its pod registers the
	// node; the manager DaemonSet is Ready by this point, so a missing
	// resource is a race worth retrying rather than a failure.
	live, err := nodes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get longhorn node %q: %w", name, err)
	}
	current, found, err := unstructured.NestedBool(live.Object, "spec", "allowScheduling")
	if err != nil {
		return terminal(fmt.Errorf("decode longhorn node %q scheduling: %w", name, err))
	}
	if !found {
		// Longhorn schedules onto a newly registered node by default.
		current = true
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
	return objects, nil
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
