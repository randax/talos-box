package provision

import (
	"context"
	"fmt"

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
// with a replica on the node and no healthy replica anywhere else.
func CountNodeStorageVolumes(ctx context.Context, kubeconfig []byte, engine cluster.CSI, nodeName string) (int, error) {
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
	return countNodeStorageVolumes(ctx, clientset, dynamicClient, engine, nodeName)
}

func countNodeStorageVolumes(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface, engine cluster.CSI, nodeName string) (int, error) {
	persistentVolumes, err := enginePersistentVolumes(ctx, client, engine)
	if err != nil {
		return 0, err
	}
	switch engine {
	case cluster.CSILocalPath:
		return countLocalPathNodeVolumes(persistentVolumes, nodeName), nil
	case cluster.CSILonghorn:
		return countLonghornNodeVolumes(ctx, dynamicClient, persistentVolumes, nodeName)
	default:
		return 0, fmt.Errorf("unsupported storage engine %q", engine)
	}
}

// enginePersistentVolumes lists the engine's PersistentVolumes the same way
// countProvisionedStorageVolumes does: keyed on spec.storageClassName, minus
// the talosbox storage probe residue.
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
			for _, value := range requirement.Values {
				if value == nodeName {
					return true
				}
			}
		}
	}
	return false
}

// countLonghornNodeVolumes counts Longhorn volumes the node holds the last
// usable copy of: a replica sits on the node and no healthy replica exists on
// any other node. A replica is healthy once it reported healthyAt without a
// later failedAt.
func countLonghornNodeVolumes(ctx context.Context, dynamicClient dynamic.Interface, volumes []*corev1.PersistentVolume, nodeName string) (int, error) {
	if len(volumes) == 0 {
		return 0, nil
	}
	replicas, err := dynamicClient.Resource(longhornReplicaResource).Namespace(longhornNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("list longhorn replicas: %w", err)
	}
	onNode := make(map[string]bool)
	healthyElsewhere := make(map[string]bool)
	for i := range replicas.Items {
		replica := &replicas.Items[i]
		volumeName := nestedString(replica, "spec", "volumeName")
		replicaNode := nestedString(replica, "spec", "nodeID")
		if volumeName == "" || replicaNode == "" {
			continue
		}
		if replicaNode == nodeName {
			onNode[volumeName] = true
			continue
		}
		healthy := nestedString(replica, "spec", "failedAt") == "" && nestedString(replica, "spec", "healthyAt") != ""
		if healthy {
			healthyElsewhere[volumeName] = true
		}
	}
	count := 0
	for _, persistentVolume := range volumes {
		if persistentVolume.Spec.CSI == nil {
			continue
		}
		volumeName := persistentVolume.Spec.CSI.VolumeHandle
		if onNode[volumeName] && !healthyElsewhere[volumeName] {
			count++
		}
	}
	return count, nil
}

func nestedString(object *unstructured.Unstructured, fields ...string) string {
	value, _, err := unstructured.NestedString(object.Object, fields...)
	if err != nil {
		return ""
	}
	return value
}
