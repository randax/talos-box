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

// ProvisionedStorageVolumeClaims names the claims behind the engine's
// PersistentVolumes as "namespace/name", sorted, so a refusal can say which
// volumes block it instead of only how many (#393). A volume whose claim
// reference is gone is named by the PersistentVolume itself — the operator
// still has to go and look at something.
func ProvisionedStorageVolumeClaims(ctx context.Context, kubeconfig []byte, engine cluster.CSI) ([]string, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig for storage volume inspection: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client for storage volume inspection: %w", err)
	}
	return provisionedStorageVolumeClaims(ctx, clientset, engine)
}

func provisionedStorageVolumeClaims(ctx context.Context, client kubernetes.Interface, engine cluster.CSI) ([]string, error) {
	persistentVolumes, err := enginePersistentVolumes(ctx, client, engine)
	if err != nil {
		return nil, err
	}
	claims := make([]string, 0, len(persistentVolumes))
	for _, persistentVolume := range persistentVolumes {
		claims = append(claims, storageVolumeClaimName(persistentVolume))
	}
	slices.Sort(claims)
	return claims, nil
}

func storageVolumeClaimName(persistentVolume *corev1.PersistentVolume) string {
	if claim := persistentVolume.Spec.ClaimRef; claim != nil && claim.Name != "" {
		return claim.Namespace + "/" + claim.Name
	}
	return persistentVolume.Name
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
		if !storageObjectOwnedByTalosbox(live, object) {
			return fmt.Errorf("StorageClass %q exists with different parameters but is not managed by talosbox; remove or rename it before provisioning curated storage", object.GetName())
		}
		if err := client.Resource(mapping.Resource).Delete(ctx, object.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("replace StorageClass %q: %w", object.GetName(), err)
		}
		name := object.GetName()
		if err := poll(ctx, GateStorageClass, interval, func(ctx context.Context) error {
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
		if found && !storageObjectOwnedByTalosbox(live, &candidate) {
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
		objects, err := renderLonghorn(cluster.Cluster{
			ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILonghorn},
			Nodes:              make([]cluster.Node, 3),
		})
		if err != nil {
			return nil, err
		}
		// The render is what tbx applied; the runtime objects are what
		// longhorn-manager added to it. Both have to go, or the engine
		// outlives the switch away from it (#386, #394).
		return append(objects, longhornRuntimeObjects()...), nil
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

func storageObjectOwnedByTalosbox(live, rendered *unstructured.Unstructured) bool {
	if live.GetLabels()[managedLabelKey] == "true" {
		return true
	}
	for _, field := range live.GetManagedFields() {
		if field.Manager == fieldManager {
			return true
		}
	}
	return storageEngineOwnedStorageClass(live, rendered) || longhornOwnedRuntimeObject(live)
}

// storageEngineOwnedStorageClass recognizes a StorageClass the curated engine
// re-created for itself. longhorn-driver-deployer rewrites the `longhorn`
// class from the longhorn-storageclass ConfigMap, and until #338 that
// definition carried no managed label — so a class tbx installed comes back
// looking user-owned and every switch away from longhorn is refused. The
// provisioner alone does not prove that history: a user can pre-create their
// own `longhorn` class with Longhorn's provisioner and customized fields, and
// that class must never be adopted for deletion. Legacy tbx classes only ever
// diverge from the rendered definition in numberOfReplicas (the one parameter
// tbx derives from topology), so ownership additionally requires the live
// parameters and reclaim policy to match the rendered ones with that key set
// aside. A customized class fails the comparison and stays refused —
// fail-closed; labeling it talosbox.dev/managed remains the escape hatch.
func storageEngineOwnedStorageClass(live, rendered *unstructured.Unstructured) bool {
	if live.GetKind() != "StorageClass" || rendered == nil {
		return false
	}
	liveProvisioner, _, err := unstructured.NestedString(live.Object, "provisioner")
	if err != nil || liveProvisioner != longhornProvisioner {
		return false
	}
	renderedProvisioner, _, err := unstructured.NestedString(rendered.Object, "provisioner")
	if err != nil || renderedProvisioner != longhornProvisioner {
		return false
	}
	liveReclaim, _, err := unstructured.NestedString(live.Object, "reclaimPolicy")
	if err != nil {
		return false
	}
	renderedReclaim, _, err := unstructured.NestedString(rendered.Object, "reclaimPolicy")
	if err != nil || liveReclaim != renderedReclaim {
		return false
	}
	liveParameters, _, err := unstructured.NestedStringMap(live.Object, "parameters")
	if err != nil {
		return false
	}
	renderedParameters, _, err := unstructured.NestedStringMap(rendered.Object, "parameters")
	if err != nil {
		return false
	}
	delete(liveParameters, "numberOfReplicas")
	delete(renderedParameters, "numberOfReplicas")
	return maps.Equal(liveParameters, renderedParameters)
}

// storageDeletionOrder tears an engine down in three moves, then the
// namespace. Everything keeps its reverse install order within a move.
//
//  1. The controllers, so nothing is left running that re-creates what the
//     next move deletes: longhorn-driver-deployer rewrites the StorageClass
//     from its ConfigMap (#338), and longhorn-manager installs its own
//     admission webhook configurations.
//  2. What the engine reaches the whole cluster with — the admission webhook
//     configurations and the StorageClasses. A Longhorn webhook outliving its
//     Service fails closed: it rejects every PVC bind in the cluster,
//     including the storage probe's, and holds the longhorn-system namespace
//     in Terminating for good (#386). A StorageClass outliving its engine
//     advertises a provisioner nothing serves (#394). Both therefore go before
//     the Service and the namespace that back them.
//  3. Everything else — Services, RBAC, config, CRDs.
func storageDeletionOrder(objects []unstructured.Unstructured) []unstructured.Unstructured {
	controllers := make([]unstructured.Unstructured, 0, len(objects))
	clusterWide := make([]unstructured.Unstructured, 0, len(objects))
	namespaces := make([]unstructured.Unstructured, 0, len(objects))
	others := make([]unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		switch {
		case object.GetKind() == "Namespace":
			namespaces = append(namespaces, object)
		case storageObjectRunsWorkload(object):
			controllers = append(controllers, object)
		case storageObjectReachesWholeCluster(object):
			clusterWide = append(clusterWide, object)
		default:
			others = append(others, object)
		}
	}
	slices.Reverse(controllers)
	slices.Reverse(clusterWide)
	slices.Reverse(others)
	slices.Reverse(namespaces)
	ordered := append(controllers, clusterWide...)
	ordered = append(ordered, others...)
	return append(ordered, namespaces...)
}

func storageObjectRunsWorkload(object unstructured.Unstructured) bool {
	switch object.GetKind() {
	case "Deployment", "DaemonSet", "StatefulSet", "ReplicaSet", "Job", "CronJob", "Pod":
		return true
	default:
		return false
	}
}

func storageObjectReachesWholeCluster(object unstructured.Unstructured) bool {
	switch object.GetKind() {
	case "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration", "StorageClass":
		return true
	default:
		return false
	}
}

func storageLifecycleSkipDelete(object unstructured.Unstructured) bool {
	switch object.GetKind() {
	case "PersistentVolume", "PersistentVolumeClaim":
		return true
	default:
		return false
	}
}
