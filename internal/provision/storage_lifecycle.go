package provision

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

const managedLabelKey = "talosbox.dev/managed"

// CountProvisionedStorageVolumes counts PersistentVolumes provisioned through
// the requested curated storage engine. It conservatively keys only on
// spec.storageClassName and excludes the known talosbox storage probe claim.
func CountProvisionedStorageVolumes(ctx context.Context, kubeconfig []byte, engine cluster.CSI) (int, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return 0, fmt.Errorf("parse kubeconfig for storage volume count: %w", err)
	}
	return CountProvisionedStorageVolumesForConfig(ctx, config, engine)
}

// CountProvisionedStorageVolumesForConfig counts PersistentVolumes provisioned
// through the requested curated storage engine using an existing REST config.
func CountProvisionedStorageVolumesForConfig(ctx context.Context, config *rest.Config, engine cluster.CSI) (int, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return 0, fmt.Errorf("create Kubernetes client for storage volume count: %w", err)
	}
	return countProvisionedStorageVolumes(ctx, clientset, engine)
}

func countProvisionedStorageVolumes(ctx context.Context, client kubernetes.Interface, engine cluster.CSI) (int, error) {
	persistentVolumes, err := enginePersistentVolumes(ctx, client, engine)
	if err != nil {
		return 0, err
	}
	return len(persistentVolumes), nil
}

// DeleteStorageEngineObjects removes the rendered Kubernetes objects for the
// requested curated storage engine only when the live objects are talosbox-
// owned. PersistentVolumes and PersistentVolumeClaims are never deleted.
func DeleteStorageEngineObjects(ctx context.Context, kubeconfig []byte, engine cluster.CSI) error {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("parse kubeconfig for storage cleanup: %w", err)
	}
	return DeleteStorageEngineObjectsForConfig(ctx, config, engine)
}

// ValidateStorageEngineObjects proves every rendered live object is safe for
// talosbox to delete without mutating the cluster.
func ValidateStorageEngineObjects(ctx context.Context, kubeconfig []byte, engine cluster.CSI) error {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("parse kubeconfig for storage cleanup validation: %w", err)
	}
	return ValidateStorageEngineObjectsForConfig(ctx, config, engine)
}

// ValidateStorageEngineObjectsForConfig validates deletion ownership and REST
// mappings using an existing REST config, without deleting any object.
func ValidateStorageEngineObjectsForConfig(ctx context.Context, config *rest.Config, engine cluster.CSI) error {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes discovery client for storage cleanup validation: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes dynamic client for storage cleanup validation: %w", err)
	}
	objects, err := storageObjectsForEngine(engine)
	if err != nil {
		return err
	}
	return validateRenderedStorageObjects(ctx, dynamicClient, mapper, objects)
}

// DeleteStorageEngineObjectsForConfig removes the rendered Kubernetes objects
// for the requested curated storage engine using an existing REST config.
func DeleteStorageEngineObjectsForConfig(ctx context.Context, config *rest.Config, engine cluster.CSI) error {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes discovery client for storage cleanup: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes dynamic client for storage cleanup: %w", err)
	}
	return deleteStorageEngineObjects(ctx, dynamicClient, mapper, engine)
}

func deleteStorageEngineObjects(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, engine cluster.CSI) error {
	objects, err := storageObjectsForEngine(engine)
	if err != nil {
		return err
	}
	return deleteRenderedStorageObjects(ctx, client, mapper, objects)
}

func deleteRenderedStorageObjects(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, objects []unstructured.Unstructured) error {
	ordered := storageDeletionOrder(objects)
	if err := validateRenderedStorageObjects(ctx, client, mapper, ordered); err != nil {
		return err
	}
	resources := make([]dynamic.ResourceInterface, 0, len(ordered))
	candidates := make([]unstructured.Unstructured, 0, len(ordered))
	for _, candidate := range ordered {
		if storageLifecycleSkipDelete(candidate) {
			continue
		}
		resource, _, found, err := getDynamicObject(ctx, client, mapper, candidate)
		if err != nil {
			return fmt.Errorf("get stale storage %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		if !found {
			resources = append(resources, nil)
			candidates = append(candidates, candidate)
			continue
		}
		resources = append(resources, resource)
		candidates = append(candidates, candidate)
	}
	for i, candidate := range candidates {
		if resources[i] == nil {
			continue
		}
		if err := resources[i].Delete(ctx, candidate.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale storage %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
	}
	return nil
}

// replaceDriftedStorageClass deletes a live StorageClass whose parameters no
// longer match the rendered ones — parameters are immutable, so a derived
// replica-count change can never be applied in place. Deleting a StorageClass
// never touches existing PersistentVolumes or claims; the following apply
// recreates it with the new parameters. Deletion is asynchronous, so the
// replacement waits until the object is actually gone before returning.
func replaceDriftedStorageClass(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, objects []unstructured.Unstructured, interval time.Duration) error {
	for i := range objects {
		object := &objects[i]
		if object.GetKind() != "StorageClass" {
			continue
		}
		mapping, err := mapper.RESTMapping(object.GroupVersionKind().GroupKind(), object.GroupVersionKind().Version)
		if err != nil {
			return fmt.Errorf("map StorageClass %q: %w", object.GetName(), err)
		}
		live, err := client.Resource(mapping.Resource).Get(ctx, object.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect StorageClass %q: %w", object.GetName(), err)
		}
		liveParameters, _, err := unstructured.NestedStringMap(live.Object, "parameters")
		if err != nil {
			return fmt.Errorf("decode live StorageClass %q parameters: %w", object.GetName(), err)
		}
		renderedParameters, _, err := unstructured.NestedStringMap(object.Object, "parameters")
		if err != nil {
			return fmt.Errorf("decode rendered StorageClass %q parameters: %w", object.GetName(), err)
		}
		if maps.Equal(liveParameters, renderedParameters) {
			continue
		}
		if !storageObjectOwnedByTalosbox(live) {
			return fmt.Errorf("StorageClass %q exists with different parameters but is not managed by talosbox; remove or rename it before provisioning curated storage", object.GetName())
		}
		if err := client.Resource(mapping.Resource).Delete(ctx, object.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("replace StorageClass %q: %w", object.GetName(), err)
		}
		name := object.GetName()
		if err := poll(ctx, interval, func(ctx context.Context) error {
			_, err := client.Resource(mapping.Resource).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return terminalError{err: fmt.Errorf("await StorageClass %q deletion: %w", name, err)}
			}
			return fmt.Errorf("StorageClass %q is still terminating", name)
		}); err != nil {
			return err
		}
	}
	return nil
}

func validateRenderedStorageObjects(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, objects []unstructured.Unstructured) error {
	for _, candidate := range storageDeletionOrder(objects) {
		if storageLifecycleSkipDelete(candidate) {
			continue
		}
		_, live, found, err := getDynamicObject(ctx, client, mapper, candidate)
		if err != nil {
			return fmt.Errorf("get stale storage %s %q: %w", candidate.GetKind(), candidate.GetName(), err)
		}
		if found && !storageObjectOwnedByTalosbox(live) {
			return fmt.Errorf("refuse to remove unmanaged storage %s %q", candidate.GetKind(), candidate.GetName())
		}
	}
	return nil
}

func storageObjectsForEngine(engine cluster.CSI) ([]unstructured.Unstructured, error) {
	switch engine {
	case cluster.CSILocalPath:
		return renderLocalPath(cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILocalPath}})
	case cluster.CSILonghorn:
		return renderLonghorn(cluster.Cluster{
			ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILonghorn},
			Nodes:              make([]cluster.Node, 3),
		})
	default:
		return nil, fmt.Errorf("unsupported storage engine %q", engine)
	}
}

func storageClassForEngine(engine cluster.CSI) (string, error) {
	switch engine {
	case cluster.CSILocalPath:
		return localPathStorageClass, nil
	case cluster.CSILonghorn:
		return longhornStorageClass, nil
	default:
		return "", fmt.Errorf("unsupported storage engine %q", engine)
	}
}

func storageProbePVResidue(persistentVolume *corev1.PersistentVolume) bool {
	return persistentVolume.Spec.ClaimRef != nil &&
		persistentVolume.Spec.ClaimRef.Namespace == probeNamespace &&
		persistentVolume.Spec.ClaimRef.Name == storageProbePVCName
}

func storageObjectOwnedByTalosbox(object *unstructured.Unstructured) bool {
	if object.GetLabels()[managedLabelKey] == "true" {
		return true
	}
	for _, field := range object.GetManagedFields() {
		if field.Manager == fieldManager {
			return true
		}
	}
	return storageEngineOwnedStorageClass(object)
}

// storageEngineOwnedStorageClass recognizes a StorageClass the curated engine
// re-creates for itself. longhorn-driver-deployer rewrites the `longhorn`
// class from the longhorn-storageclass ConfigMap, and until #338 that
// definition carried no managed label — so a class tbx installed comes back
// looking user-owned and every switch away from longhorn is refused. Only
// classes rendered for the engine being removed are ever checked here, so a
// class carrying Longhorn's own provisioner belongs to that engine and never
// to a user: a user-owned class would have to answer to a different name, and
// a different name is not a candidate for removal in the first place.
func storageEngineOwnedStorageClass(object *unstructured.Unstructured) bool {
	if object.GetKind() != "StorageClass" {
		return false
	}
	provisioner, _, err := unstructured.NestedString(object.Object, "provisioner")
	if err != nil {
		return false
	}
	return provisioner == longhornProvisioner
}

func storageDeletionOrder(objects []unstructured.Unstructured) []unstructured.Unstructured {
	namespaces := make([]unstructured.Unstructured, 0, len(objects))
	others := make([]unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		if object.GetKind() == "Namespace" {
			namespaces = append(namespaces, object)
			continue
		}
		others = append(others, object)
	}
	slices.Reverse(others)
	slices.Reverse(namespaces)
	return append(others, namespaces...)
}

func storageLifecycleSkipDelete(object unstructured.Unstructured) bool {
	switch object.GetKind() {
	case "PersistentVolume", "PersistentVolumeClaim":
		return true
	default:
		return false
	}
}
