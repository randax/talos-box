package provision

import (
	"context"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func localPathPV(name, node string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: localPathStorageClass,
			ClaimRef:         &corev1.ObjectReference{Namespace: "app", Name: name},
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "kubernetes.io/hostname",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{node},
						}},
					}},
				},
			},
		},
	}
}

func TestCountNodeStorageVolumesLocalPathCountsOnlyNodePinnedPVs(t *testing.T) {
	probe := localPathPV("probe", "demo-worker-1")
	probe.Spec.ClaimRef = &corev1.ObjectReference{Namespace: probeNamespace, Name: storageProbePVCName}
	otherClass := localPathPV("longhorn-pv", "demo-worker-1")
	otherClass.Spec.StorageClassName = longhornStorageClass
	client := kubernetesfake.NewClientset(
		localPathPV("on-node", "demo-worker-1"),
		localPathPV("also-on-node", "demo-worker-1"),
		localPathPV("elsewhere", "demo-worker-2"),
		probe,
		otherClass,
	)

	count, err := countNodeStorageVolumes(context.Background(), client, nil, cluster.CSILocalPath, "demo-worker-1", []string{"demo-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("countNodeStorageVolumes(local-path) = %d, want 2", count)
	}
}

func longhornPV(name, volumeHandle string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: longhornStorageClass,
			ClaimRef:         &corev1.ObjectReference{Namespace: "app", Name: name},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "driver.longhorn.io", VolumeHandle: volumeHandle},
			},
		},
	}
}

func longhornReplica(name, volume, node, failedAt, healthyAt string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "longhorn.io/v1beta2",
		"kind":       "Replica",
		"metadata":   map[string]any{"name": name, "namespace": longhornNamespace},
		"spec": map[string]any{
			"volumeName": volume,
			"nodeID":     node,
			"failedAt":   failedAt,
			"healthyAt":  healthyAt,
		},
	}}
}

func fakeLonghornReplicas(replicas ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	objects := make([]runtime.Object, 0, len(replicas))
	for _, replica := range replicas {
		objects = append(objects, replica)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{longhornReplicaResource: "ReplicaList"},
		objects...,
	)
}

func TestCountNodeStorageVolumesLonghornCountsVolumesWithoutHealthyReplicaElsewhere(t *testing.T) {
	probe := longhornPV("probe", "pvc-probe")
	probe.Spec.ClaimRef = &corev1.ObjectReference{Namespace: probeNamespace, Name: storageProbePVCName}
	client := kubernetesfake.NewClientset(
		longhornPV("lone", "pvc-lone"),           // only healthy replica on the node -> at risk
		longhornPV("replicated", "pvc-repl"),     // healthy replica elsewhere -> safe
		longhornPV("degraded", "pvc-degraded"),   // replica on node, others failed -> at risk
		longhornPV("elsewhere", "pvc-elsewhere"), // no replica on the node -> safe
		probe,
	)
	dynamicClient := fakeLonghornReplicas(
		longhornReplica("lone-r1", "pvc-lone", "demo-worker-1", "", "2026-01-01T00:00:00Z"),
		longhornReplica("repl-r1", "pvc-repl", "demo-worker-1", "", "2026-01-01T00:00:00Z"),
		longhornReplica("repl-r2", "pvc-repl", "demo-worker-2", "", "2026-01-01T00:00:00Z"),
		longhornReplica("degraded-r1", "pvc-degraded", "demo-worker-1", "", "2026-01-01T00:00:00Z"),
		longhornReplica("degraded-r2", "pvc-degraded", "demo-worker-2", "2026-01-02T00:00:00Z", "2026-01-01T00:00:00Z"),
		longhornReplica("elsewhere-r1", "pvc-elsewhere", "demo-worker-2", "", "2026-01-01T00:00:00Z"),
		longhornReplica("probe-r1", "pvc-probe", "demo-worker-1", "", "2026-01-01T00:00:00Z"),
	)

	count, err := countNodeStorageVolumes(context.Background(), client, dynamicClient, cluster.CSILonghorn, "demo-worker-1", []string{"demo-cp-1", "demo-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("countNodeStorageVolumes(longhorn) = %d, want 2 (lone + degraded)", count)
	}
}

func TestCountNodeStorageVolumesLonghornCountsRebuildingOnlyReplicaOnNode(t *testing.T) {
	client := kubernetesfake.NewClientset(longhornPV("rebuilding", "pvc-rebuilding"))
	dynamicClient := fakeLonghornReplicas(
		// never became healthy, but it is the only copy on any node
		longhornReplica("rebuilding-r1", "pvc-rebuilding", "demo-worker-1", "", ""),
	)

	count, err := countNodeStorageVolumes(context.Background(), client, dynamicClient, cluster.CSILonghorn, "demo-worker-1", []string{"demo-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("countNodeStorageVolumes(longhorn rebuilding-only) = %d, want 1", count)
	}
}

func TestCountNodeStorageVolumesLonghornCountsVolumeWithoutPersistentVolume(t *testing.T) {
	// A Longhorn volume can outlive (or never have) its PV — UI-created,
	// orphaned, or retained. Replica CRs, not PVs, define the candidates.
	client := kubernetesfake.NewClientset()
	dynamicClient := fakeLonghornReplicas(
		longhornReplica("orphan-r1", "pvc-orphan", "demo-worker-1", "", "2026-01-01T00:00:00Z"),
	)

	count, err := countNodeStorageVolumes(context.Background(), client, dynamicClient, cluster.CSILonghorn, "demo-worker-1", []string{"demo-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("countNodeStorageVolumes(longhorn orphan) = %d, want 1", count)
	}
}

func TestCountNodeStorageVolumesLonghornIgnoresReplicasOutsideRemainingNodes(t *testing.T) {
	// A healthy replica CR left behind on an already-removed node must not
	// count as a surviving copy.
	client := kubernetesfake.NewClientset(longhornPV("stale", "pvc-stale"))
	dynamicClient := fakeLonghornReplicas(
		longhornReplica("stale-r1", "pvc-stale", "demo-worker-1", "", "2026-01-01T00:00:00Z"),
		longhornReplica("stale-r2", "pvc-stale", "demo-worker-9", "", "2026-01-01T00:00:00Z"),
	)

	count, err := countNodeStorageVolumes(context.Background(), client, dynamicClient, cluster.CSILonghorn, "demo-worker-1", []string{"demo-cp-1"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("countNodeStorageVolumes(longhorn stale elsewhere) = %d, want 1", count)
	}
}

func TestCountNodeStorageVolumesLonghornIgnoresTerminatingReplicaElsewhere(t *testing.T) {
	terminating := longhornReplica("term-r2", "pvc-term", "demo-worker-2", "", "2026-01-01T00:00:00Z")
	now := metav1.Now()
	terminating.SetDeletionTimestamp(&now)
	client := kubernetesfake.NewClientset(longhornPV("term", "pvc-term"))
	dynamicClient := fakeLonghornReplicas(
		longhornReplica("term-r1", "pvc-term", "demo-worker-1", "", "2026-01-01T00:00:00Z"),
		terminating,
	)

	count, err := countNodeStorageVolumes(context.Background(), client, dynamicClient, cluster.CSILonghorn, "demo-worker-1", []string{"demo-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("countNodeStorageVolumes(longhorn terminating elsewhere) = %d, want 1", count)
	}
}

func TestCountNodeStorageVolumesLonghornIgnoresInactiveReplicaElsewhere(t *testing.T) {
	// An inactive replica (leftover from an engine live-upgrade) may be
	// stale; only an active healthy replica counts as a surviving copy.
	inactive := longhornReplica("upg-r2", "pvc-upg", "demo-worker-2", "", "2026-01-01T00:00:00Z")
	if err := unstructured.SetNestedField(inactive.Object, false, "spec", "active"); err != nil {
		t.Fatal(err)
	}
	client := kubernetesfake.NewClientset(longhornPV("upg", "pvc-upg"))
	dynamicClient := fakeLonghornReplicas(
		longhornReplica("upg-r1", "pvc-upg", "demo-worker-1", "", "2026-01-01T00:00:00Z"),
		inactive,
	)

	count, err := countNodeStorageVolumes(context.Background(), client, dynamicClient, cluster.CSILonghorn, "demo-worker-1", []string{"demo-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("countNodeStorageVolumes(longhorn inactive elsewhere) = %d, want 1", count)
	}
}

func TestCountNodeStorageVolumesLonghornExcludesProbeResidue(t *testing.T) {
	probe := longhornPV("probe", "pvc-probe")
	probe.Spec.ClaimRef = &corev1.ObjectReference{Namespace: probeNamespace, Name: storageProbePVCName}
	client := kubernetesfake.NewClientset(probe)
	dynamicClient := fakeLonghornReplicas(
		longhornReplica("probe-r1", "pvc-probe", "demo-worker-1", "", "2026-01-01T00:00:00Z"),
	)

	count, err := countNodeStorageVolumes(context.Background(), client, dynamicClient, cluster.CSILonghorn, "demo-worker-1", []string{"demo-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("countNodeStorageVolumes(longhorn probe residue) = %d, want 0", count)
	}
}

func TestCountNodeStorageVolumesRejectsUnknownEngine(t *testing.T) {
	client := kubernetesfake.NewClientset()
	if _, err := countNodeStorageVolumes(context.Background(), client, nil, cluster.CSI("zfs"), "demo-worker-1", nil); err == nil {
		t.Fatal("countNodeStorageVolumes(zfs) succeeded, want unsupported engine error")
	}
}
