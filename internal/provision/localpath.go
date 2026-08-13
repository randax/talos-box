package provision

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/randax/talos-box/internal/cluster"
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
	localPathVersion          = "0.0.37"
	localPathManifestSHA256   = "5e25493166b1b89a0062a4a277c53a39fc7153d86194ade49a80241b37dd1a8e"
	localPathNamespace        = "local-path-storage"
	localPathProvisionerName  = "local-path-provisioner"
	localPathStorageClass     = "local-path"
	localPathConfigMap        = "local-path-config"
	localPathProvisionerImage = "docker.io/rancher/local-path-provisioner:v0.0.37"
	localPathHelperImage      = "docker.io/library/busybox:1.37.0"
	localPathNodePath         = "/var/local-path-provisioner"
)

//go:embed assets/local-path-storage-0.0.37.yaml
var localPathManifest []byte

// StorageResult is the verified storage reconcile narration after the host-side
// SSA flow completes.
type StorageResult struct {
	Narration []string
	Phase     StoragePhase
	Live      bool
}

// LocalPathReconciler installs and verifies the pinned local-path
// provisioner through the host-side render/apply path.
type LocalPathReconciler struct {
	PollInterval time.Duration
}

func (r LocalPathReconciler) Reconcile(ctx context.Context, item cluster.Cluster, kubeconfig []byte) (StorageResult, error) {
	objects, err := renderLocalPath(item)
	if err != nil {
		return StorageResult{}, err
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return StorageResult{}, fmt.Errorf("parse kubeconfig for Kubernetes apply: %w", err)
	}
	return r.reconcile(ctx, config, objects)
}

func (r LocalPathReconciler) reconcile(ctx context.Context, config *rest.Config, objects []unstructured.Unstructured) (StorageResult, error) {
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

	namespaces, manifestObjects := partitionLocalPathObjects(objects)
	if err := applyAll(ctx, dynamicClient, mapper, namespaces); err != nil {
		return StorageResult{}, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, manifestObjects); err != nil {
		return StorageResult{}, err
	}
	if err := waitForLocalPath(ctx, clientset, r.PollInterval); err != nil {
		return StorageResult{}, err
	}
	if err := runStorageProbe(ctx, dynamicClient, mapper, clientset, storageProbeSpec{
		ProbeImage: localPathHelperImage,
	}, r.PollInterval); err != nil {
		return StorageResult{}, err
	}
	return StorageResult{Narration: []string{
		"≈ kubectl apply --server-side -f - # local-path-provisioner v" + localPathVersion,
		"≈ kubectl apply --server-side -f - # storage probe PVC + writer/reader pods",
	}, Phase: StoragePhaseLive, Live: true}, nil
}

func renderLocalPath(item cluster.Cluster) ([]unstructured.Unstructured, error) {
	if item.CSI != cluster.CSILocalPath {
		return nil, errors.New("local-path rendering requires csi: local-path")
	}
	if actual := fmt.Sprintf("%x", sha256.Sum256(localPathManifest)); actual != localPathManifestSHA256 {
		return nil, fmt.Errorf("embedded local-path manifest checksum = %s, want %s", actual, localPathManifestSHA256)
	}
	objects, err := decodeObjects(localPathManifest)
	if err != nil {
		return nil, fmt.Errorf("decode embedded local-path manifest: %w", err)
	}
	for i := range objects {
		object := &objects[i]
		ensureManagedLabel(object)
		switch object.GetKind() {
		case "Namespace":
			if object.GetName() == localPathNamespace {
				ensureNamespaceLabel(object, "pod-security.kubernetes.io/enforce", "privileged")
				ensureNamespaceLabel(object, "pod-security.kubernetes.io/audit", "privileged")
				ensureNamespaceLabel(object, "pod-security.kubernetes.io/warn", "privileged")
			}
		case "Deployment":
			if object.GetName() == localPathProvisionerName && object.GetNamespace() == localPathNamespace {
				if err := setContainerImage(object, localPathProvisionerImage); err != nil {
					return nil, fmt.Errorf("pin local-path provisioner image: %w", err)
				}
			}
		case "StorageClass":
			if object.GetName() == localPathStorageClass {
				ensureAnnotation(object, "storageclass.kubernetes.io/is-default-class", "true")
				ensureAnnotation(object, "storageclass.beta.kubernetes.io/is-default-class", "true")
			}
		case "ConfigMap":
			if object.GetName() == localPathConfigMap && object.GetNamespace() == localPathNamespace {
				if err := unstructured.SetNestedStringMap(object.Object, map[string]string{
					"config.json":    localPathConfigJSON,
					"setup":          localPathSetupScript,
					"teardown":       localPathTeardownScript,
					"helperPod.yaml": localPathHelperPodYAML,
				}, "data"); err != nil {
					return nil, fmt.Errorf("rewrite local-path config map: %w", err)
				}
			}
		}
	}
	return objects, nil
}

func partitionLocalPathObjects(objects []unstructured.Unstructured) (namespaces, manifestObjects []unstructured.Unstructured) {
	for _, object := range objects {
		if object.GetKind() == "Namespace" {
			namespaces = append(namespaces, object)
			continue
		}
		manifestObjects = append(manifestObjects, object)
	}
	return namespaces, manifestObjects
}

func waitForLocalPath(ctx context.Context, client kubernetes.Interface, interval time.Duration) error {
	return poll(ctx, interval, func(ctx context.Context) error {
		deployment, err := client.AppsV1().Deployments(localPathNamespace).Get(ctx, localPathProvisionerName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !deploymentReady(deployment) {
			return errors.New("local-path provisioner deployment is not Ready")
		}
		return nil
	})
}

func ensureManagedLabel(object *unstructured.Unstructured) {
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["talosbox.dev/managed"] = "true"
	object.SetLabels(labels)
}

func ensureNamespaceLabel(object *unstructured.Unstructured, key, value string) {
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[key] = value
	object.SetLabels(labels)
}

func ensureAnnotation(object *unstructured.Unstructured, key, value string) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[key] = value
	object.SetAnnotations(annotations)
}

func setContainerImage(object *unstructured.Unstructured, image string) error {
	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		return fmt.Errorf("container list missing")
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return fmt.Errorf("container entry = %T", containers[0])
	}
	container["image"] = image
	containers[0] = container
	return unstructured.SetNestedSlice(object.Object, containers, "spec", "template", "spec", "containers")
}

const localPathConfigJSON = `{
  "nodePathMap": [
    {
      "node": "DEFAULT_PATH_FOR_NON_LISTED_NODES",
      "paths": [
        "/var/local-path-provisioner"
      ]
    }
  ]
}`

const localPathSetupScript = `#!/bin/sh
set -eu
mkdir -m 0777 -p "$VOL_DIR"
`

const localPathTeardownScript = `#!/bin/sh
set -eu
rm -rf "$VOL_DIR"
`

const localPathHelperPodYAML = `apiVersion: v1
kind: Pod
metadata:
  name: helper-pod
spec:
  priorityClassName: system-node-critical
  tolerations:
    - key: node.kubernetes.io/disk-pressure
      operator: Exists
      effect: NoSchedule
  containers:
    - name: helper-pod
      image: docker.io/library/busybox:1.37.0
`
