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
	return r.reconcile(ctx, config, objects)
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

func (r LonghornReconciler) reconcile(ctx context.Context, config *rest.Config, objects []unstructured.Unstructured) (StorageResult, error) {
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
	if err := applyAll(ctx, dynamicClient, mapper, chartObjects); err != nil {
		return StorageResult{}, err
	}
	if err := waitForLonghorn(ctx, clientset, r.PollInterval); err != nil {
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
	replicas := longhornReplicaCount(len(item.Nodes))
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

func longhornValues(replicas int) string {
	return fmt.Sprintf(`persistence:
  defaultClass: true
  defaultClassReplicaCount: %d
defaultSettings:
  defaultDataPath: /var/lib/longhorn
  defaultReplicaCount: "%d"
`, replicas, replicas)
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
