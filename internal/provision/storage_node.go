package provision

import (
	"context"
	"fmt"
	"slices"

	"github.com/randax/talos-box/internal/cluster"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var longhornReplicaResource = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "replicas"}

// CountNodeStorageVolumes counts curated-engine volumes whose data lives only
// on the named node, i.e. the volumes a `tbx node remove` would destroy: every
// local-path PersistentVolume pinned to the node, and every Longhorn volume
// with a replica on the node and no healthy replica on any remaining cluster
// node. remainingNodes are the cluster's node names minus the one being
// removed — a replica on a node outside that set (already removed, or never a
// member) cannot be trusted as a surviving copy.
func CountNodeStorageVolumes(ctx context.Context, kubeconfig []byte, engine cluster.CSI, nodeName string, remainingNodes []string) (int, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return 0, fmt.Errorf("parse kubeconfig for node volume count: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return 0, fmt.Errorf("create Kubernetes client for node volume count: %w", err)
	}
	var dynamicClient dynamic.Interface
	if engine == cluster.CSILonghorn {
		dynamicClient, err = dynamic.NewForConfig(config)
		if err != nil {
			return 0, fmt.Errorf("create Kubernetes dynamic client for node volume count: %w", err)
		}
	}
	return countNodeStorageVolumes(ctx, clientset, dynamicClient, engine, nodeName, remainingNodes)
}

func countNodeStorageVolumes(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface, engine cluster.CSI, nodeName string, remainingNodes []string) (int, error) {
	switch engine {
	case cluster.CSILocalPath:
		volumes, err := enginePersistentVolumes(ctx, client, engine)
		if err != nil {
			return 0, err
		}
		return countLocalPathNodeVolumes(volumes, nodeName), nil
	case cluster.CSILonghorn:
		return countLonghornNodeVolumes(ctx, client, dynamicClient, nodeName, remainingNodes)
	default:
		return 0, fmt.Errorf("unsupported storage engine %q", engine)
	}
}

// enginePersistentVolumes lists the engine's PersistentVolumes keyed on
// spec.storageClassName, minus the talosbox storage probe residue.
func enginePersistentVolumes(ctx context.Context, client kubernetes.Interface, engine cluster.CSI) ([]*corev1.PersistentVolume, error) {
	storageClassName, err := storageClassForEngine(engine)
	if err != nil {
		return nil, err
	}
	list, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s persistent volumes: %w", engine, err)
	}
	volumes := make([]*corev1.PersistentVolume, 0, len(list.Items))
	for i := range list.Items {
		persistentVolume := &list.Items[i]
		if persistentVolume.Spec.StorageClassName != storageClassName {
			continue
		}
		if storageProbePVResidue(persistentVolume) {
			continue
		}
		volumes = append(volumes, persistentVolume)
	}
	return volumes, nil
}

// countLocalPathNodeVolumes counts PersistentVolumes pinned to the node.
// local-path data has exactly one copy, on the node the provisioner's
// node-affinity term names.
func countLocalPathNodeVolumes(volumes []*corev1.PersistentVolume, nodeName string) int {
	count := 0
	for _, persistentVolume := range volumes {
		if persistentVolumePinnedToNode(persistentVolume, nodeName) {
			count++
		}
	}
	return count
}

func persistentVolumePinnedToNode(persistentVolume *corev1.PersistentVolume, nodeName string) bool {
	affinity := persistentVolume.Spec.NodeAffinity
	if affinity == nil || affinity.Required == nil {
		return false
	}
	for _, term := range affinity.Required.NodeSelectorTerms {
		for _, requirement := range term.MatchExpressions {
			if requirement.Key != corev1.LabelHostname || requirement.Operator != corev1.NodeSelectorOpIn {
				continue
			}
			if slices.Contains(requirement.Values, nodeName) {
				return true
			}
		}
	}
	return false
}

// countLonghornNodeVolumes counts Longhorn volumes the node holds the last
// usable copy of: a replica sits on the node and no healthy replica exists on
// any remaining cluster node. Candidates come from the replica CRs themselves
// — a volume without a PersistentVolume (UI-created, orphaned, retained)
// still holds data — with the talosbox probe volume exempted via its PV claim
// reference. A replica counts as a surviving copy only when it reported
// healthyAt without a later failedAt and is not terminating.
func countLonghornNodeVolumes(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface, nodeName string, remainingNodes []string) (int, error) {
	probeVolumes, err := longhornProbeVolumeHandles(ctx, client)
	if err != nil {
		return 0, err
	}
	replicas, err := dynamicClient.Resource(longhornReplicaResource).Namespace(longhornNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("list longhorn replicas: %w", err)
	}
	onNode := make(map[string]bool)
	survivorElsewhere := make(map[string]bool)
	for i := range replicas.Items {
		replica := &replicas.Items[i]
		volumeName := nestedString(replica, "spec", "volumeName")
		replicaNode := nestedString(replica, "spec", "nodeID")
		if volumeName == "" || replicaNode == "" || probeVolumes[volumeName] {
			continue
		}
		if replicaNode == nodeName {
			onNode[volumeName] = true
			continue
		}
		if !slices.Contains(remainingNodes, replicaNode) {
			continue
		}
		healthy := replica.GetDeletionTimestamp() == nil &&
			replicaActive(replica) &&
			nestedString(replica, "spec", "failedAt") == "" &&
			nestedString(replica, "spec", "healthyAt") != ""
		if healthy {
			survivorElsewhere[volumeName] = true
		}
	}
	count := 0
	for volumeName := range onNode {
		if !survivorElsewhere[volumeName] {
			count++
		}
	}
	return count, nil
}

// longhornProbeVolumeHandles maps the talosbox storage probe's Longhorn
// volume names, so replica-derived counting can exempt probe residue the same
// way PV-derived counting does.
func longhornProbeVolumeHandles(ctx context.Context, client kubernetes.Interface) (map[string]bool, error) {
	list, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list longhorn persistent volumes: %w", err)
	}
	handles := make(map[string]bool)
	for i := range list.Items {
		persistentVolume := &list.Items[i]
		if !storageProbePVResidue(persistentVolume) || persistentVolume.Spec.CSI == nil {
			continue
		}
		handles[persistentVolume.Spec.CSI.VolumeHandle] = true
	}
	return handles, nil
}

// replicaActive reports whether the replica is the volume's live copy rather
// than a leftover from an engine upgrade. A missing active field counts as
// active: CRD shapes that predate the field only ever described live
// replicas.
func replicaActive(replica *unstructured.Unstructured) bool {
	active, found, err := unstructured.NestedBool(replica.Object, "spec", "active")
	if err != nil || !found {
		return true
	}
	return active
}

func nestedString(object *unstructured.Unstructured, fields ...string) string {
	value, _, err := unstructured.NestedString(object.Object, fields...)
	if err != nil {
		return ""
	}
	return value
}
