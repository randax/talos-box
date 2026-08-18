package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestRenderLocalPathUsesPinnedProvisionerAssets(t *testing.T) {
	objects, err := renderLocalPath(cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath}})
	if err != nil {
		t.Fatal(err)
	}
	var (
		namespace    *unstructured.Unstructured
		deployment   *unstructured.Unstructured
		storageClass *unstructured.Unstructured
		configMap    *unstructured.Unstructured
	)
	for i := range objects {
		object := &objects[i]
		switch {
		case object.GetKind() == "Namespace" && object.GetName() == localPathNamespace:
			namespace = object
		case object.GetKind() == "Deployment" && object.GetName() == localPathProvisionerName:
			deployment = object
		case object.GetKind() == "StorageClass" && object.GetName() == localPathStorageClass:
			storageClass = object
		case object.GetKind() == "ConfigMap" && object.GetName() == localPathConfigMap:
			configMap = object
		}
		if labels := object.GetLabels(); labels["talosbox.dev/managed"] != "true" {
			t.Fatalf("%s/%s labels = %v, want talosbox.dev/managed", object.GetKind(), object.GetName(), labels)
		}
	}
	if namespace == nil || deployment == nil || storageClass == nil || configMap == nil {
		t.Fatalf("renderLocalPath() missing required objects: namespace=%v deployment=%v storageClass=%v configMap=%v", namespace != nil, deployment != nil, storageClass != nil, configMap != nil)
	}
	if got, _, err := nestedStringField(namespace.Object, "metadata", "labels", "pod-security.kubernetes.io/enforce"); err != nil || got != "privileged" {
		t.Fatalf("namespace PSA enforce = %q, %v", got, err)
	}
	if got, _, err := nestedStringField(deployment.Object, "spec", "template", "spec", "containers", "0", "image"); err != nil || got != localPathProvisionerImage {
		t.Fatalf("local-path image = %q, %v", got, err)
	}
	if got, _, err := nestedStringField(storageClass.Object, "metadata", "annotations", "storageclass.kubernetes.io/is-default-class"); err != nil || got != "true" {
		t.Fatalf("default StorageClass annotation = %q, %v", got, err)
	}
	data, found, err := unstructured.NestedStringMap(configMap.Object, "data")
	if err != nil || !found {
		t.Fatalf("configMap data = %v, %v", found, err)
	}
	if !strings.Contains(data["config.json"], localPathNodePath) {
		t.Fatalf("config.json = %q, want %s", data["config.json"], localPathNodePath)
	}
	if strings.Contains(data["config.json"], "/opt/local-path-provisioner") {
		t.Fatalf("config.json retained upstream path: %q", data["config.json"])
	}
	if !strings.Contains(data["helperPod.yaml"], localPathHelperImage) {
		t.Fatalf("helperPod.yaml = %q, want %s", data["helperPod.yaml"], localPathHelperImage)
	}
}

func TestRenderLocalPathRejectsNonLocalPathIntent(t *testing.T) {
	_, err := renderLocalPath(cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn}})
	if err == nil || !strings.Contains(err.Error(), "csi: local-path") {
		t.Fatalf("renderLocalPath() error = %v", err)
	}
}

func TestWaitForLocalPathRequiresReadyDeployment(t *testing.T) {
	ready := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: localPathProvisionerName, Namespace: localPathNamespace, Generation: 1},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	if err := waitForLocalPath(context.Background(), kubernetesfake.NewClientset(ready), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	notReady := ready.DeepCopy()
	notReady.Status.ReadyReplicas = 0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := waitForLocalPath(ctx, kubernetesfake.NewClientset(notReady), time.Millisecond); err == nil {
		t.Fatal("waitForLocalPath() accepted an unready deployment")
	}
}

func TestRunStorageProbeWritesReadsAndCleansUp(t *testing.T) {
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	reactor := recordApplyReactor(t)
	dynamicClient.PrependReactor("patch", "namespaces", reactor)
	dynamicClient.PrependReactor("patch", "persistentvolumeclaims", reactor)
	dynamicClient.PrependReactor("patch", "pods", reactor)

	mapper := storageProbeRESTMapper()
	clientset := kubernetesfake.NewClientset()
	writerGets, readerGets := 0, 0
	deleteKeys := []string{}
	deleted := map[string]bool{}
	clientset.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		if deleted["pvc/"+storageProbePVCName] {
			deleted["pvc/"+storageProbePVCName] = false
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, storageProbePVCName)
		}
		return true, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: storageProbePVCName, Namespace: probeNamespace},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: stringPointer(localPathStorageClass)},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}, nil
	})
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get := action.(k8stesting.GetAction)
		if deleted["pod/"+get.GetName()] {
			deleted["pod/"+get.GetName()] = false
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, get.GetName())
		}
		switch get.GetName() {
		case storageProbeWriterPodName:
			writerGets++
			phase := corev1.PodPending
			if writerGets > 1 {
				phase = corev1.PodSucceeded
			}
			return true, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: storageProbeWriterPodName, Namespace: probeNamespace}, Status: corev1.PodStatus{Phase: phase}}, nil
		case storageProbeReaderPodName:
			readerGets++
			return true, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: storageProbeReaderPodName, Namespace: probeNamespace}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}, nil
		default:
			return true, nil, errors.New("unexpected pod")
		}
	})
	clientset.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction := action.(k8stesting.DeleteAction)
		deleteKeys = append(deleteKeys, "pod/"+deleteAction.GetName())
		deleted["pod/"+deleteAction.GetName()] = true
		return true, nil, nil
	})
	clientset.PrependReactor("delete", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction := action.(k8stesting.DeleteAction)
		deleteKeys = append(deleteKeys, "pvc/"+deleteAction.GetName())
		deleted["pvc/"+deleteAction.GetName()] = true
		return true, nil, nil
	})

	if _, err := runStorageProbe(context.Background(), dynamicClient, mapper, clientset, storageProbeSpec{
		ExpectedStorageClass: localPathStorageClass,
		ProbeImage:           localPathHelperImage,
	}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if readerGets == 0 || writerGets < 2 {
		t.Fatalf("writerGets=%d readerGets=%d, want staged writer then reader", writerGets, readerGets)
	}
	if !slices.Contains(deleteKeys, "pod/"+storageProbeWriterPodName) || !slices.Contains(deleteKeys, "pod/"+storageProbeReaderPodName) || !slices.Contains(deleteKeys, "pvc/"+storageProbePVCName) {
		t.Fatalf("cleanup deletes = %v", deleteKeys)
	}
	patches := dynamicClient.Actions()
	if len(patches) != 4 {
		t.Fatalf("apply actions = %v", patches)
	}
}

func TestRunStorageProbeReturnsContextAndCleansUp(t *testing.T) {
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	reactor := recordApplyReactor(t)
	dynamicClient.PrependReactor("patch", "namespaces", reactor)
	dynamicClient.PrependReactor("patch", "persistentvolumeclaims", reactor)
	dynamicClient.PrependReactor("patch", "pods", reactor)

	mapper := storageProbeRESTMapper()
	clientset := kubernetesfake.NewClientset()
	deleteKeys := []string{}
	deleted := map[string]bool{}
	clientset.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		if deleted["pvc/"+storageProbePVCName] {
			deleted["pvc/"+storageProbePVCName] = false
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, storageProbePVCName)
		}
		return true, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: storageProbePVCName, Namespace: probeNamespace},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		}, nil
	})
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get := action.(k8stesting.GetAction)
		if deleted["pod/"+get.GetName()] {
			deleted["pod/"+get.GetName()] = false
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, get.GetName())
		}
		return true, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: get.GetName(), Namespace: probeNamespace}, Status: corev1.PodStatus{Phase: corev1.PodPending}}, nil
	})
	clientset.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction := action.(k8stesting.DeleteAction)
		deleteKeys = append(deleteKeys, "pod/"+deleteAction.GetName())
		deleted["pod/"+deleteAction.GetName()] = true
		return true, nil, nil
	})
	clientset.PrependReactor("delete", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction := action.(k8stesting.DeleteAction)
		deleteKeys = append(deleteKeys, "pvc/"+deleteAction.GetName())
		deleted["pvc/"+deleteAction.GetName()] = true
		return true, nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := runStorageProbe(ctx, dynamicClient, mapper, clientset, storageProbeSpec{
		ExpectedStorageClass: localPathStorageClass,
		ProbeImage:           localPathHelperImage,
	}, time.Millisecond)
	if err == nil {
		t.Fatal("runStorageProbe() error = nil, want context deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runStorageProbe() error = %v, want deadline exceeded", err)
	}
	if !slices.Contains(deleteKeys, "pod/"+storageProbeWriterPodName) || !slices.Contains(deleteKeys, "pvc/"+storageProbePVCName) {
		t.Fatalf("cleanup deletes = %v", deleteKeys)
	}
}

// A teardown that outran its bound is not a failed probe: the data path
// verified, the objects are deleting, and the daemon reruns this cleanup at the
// head of its next probe. Failing the verb here contradicted the state the
// cluster converged to minutes later (#347).
func TestRunStorageProbeReportsUnfinishedCleanupAsWarning(t *testing.T) {
	shortenStorageProbeCleanup(t)
	fixture := newStorageProbeStallFixture(t, false)

	outcome, err := runStorageProbe(context.Background(), fixture.dynamicClient, fixture.mapper, fixture.clientset, storageProbeSpec{
		ExpectedStorageClass: localPathStorageClass,
		ProbeImage:           localPathHelperImage,
		Engine:               cluster.CSILocalPath,
		ClusterName:          "demo",
	}, time.Millisecond)
	if err != nil {
		t.Fatalf("runStorageProbe() error = %v, want the unfinished cleanup reported as a warning", err)
	}
	if !outcome.Verified {
		t.Fatal("runStorageProbe() reported the data path as unverified, but the probe read its payload back")
	}
	if len(outcome.Warnings) != 1 || !strings.Contains(outcome.Warnings[0], "storage probe cleanup is still finishing") {
		t.Fatalf("runStorageProbe() warnings = %v, want an unfinished-cleanup advisory", outcome.Warnings)
	}
	if !strings.Contains(outcome.Warnings[0], "tbx status demo") {
		t.Fatalf("warning %q does not name the cluster whose status the operator can watch", outcome.Warnings[0])
	}
}

// The head cleanup answers to the same rule as the trailing one, and it is the
// path the previous warning sends the operator down: the status probe reruns
// against the very PVC that is still terminating. Hard-failing there reported
// the daemon's own pending work as a storage fault (#347).
func TestRunStorageProbeSkipsPassWhilePreviousCleanupIsPending(t *testing.T) {
	shortenStorageProbeCleanup(t)
	fixture := newStorageProbeStallFixture(t, true)

	outcome, err := runStorageProbe(context.Background(), fixture.dynamicClient, fixture.mapper, fixture.clientset, storageProbeSpec{
		ExpectedStorageClass: localPathStorageClass,
		ProbeImage:           localPathHelperImage,
		Engine:               cluster.CSILocalPath,
		ClusterName:          "demo",
	}, time.Millisecond)
	if err != nil {
		t.Fatalf("runStorageProbe() error = %v, want the pending cleanup reported as a warning", err)
	}
	if outcome.Verified {
		t.Fatal("runStorageProbe() claimed a verified data path without probing one")
	}
	if len(outcome.Warnings) != 1 || !strings.Contains(outcome.Warnings[0], "storage probe cleanup is still finishing") {
		t.Fatalf("runStorageProbe() warnings = %v, want a pending-cleanup advisory", outcome.Warnings)
	}
	if *fixture.applies != 0 {
		t.Fatalf("runStorageProbe() applied %d object(s) on top of a still-terminating PVC", *fixture.applies)
	}
}

// A deadline is only benign on its own. Joined with a real API failure it is
// the least interesting half of the error, and downgrading the pair to an
// advisory would hide a storage fault behind the daemon's own pending work.
func TestRunStorageProbeFailsWhenHeadCleanupHitsARealError(t *testing.T) {
	shortenStorageProbeCleanup(t)
	fixture := newStorageProbeStallFixture(t, true)
	fixture.clientset.PrependReactor("list", "persistentvolumes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd is unavailable"))
	})

	outcome, err := runStorageProbe(context.Background(), fixture.dynamicClient, fixture.mapper, fixture.clientset, storageProbeSpec{
		ExpectedStorageClass: localPathStorageClass,
		ProbeImage:           localPathHelperImage,
		Engine:               cluster.CSILocalPath,
		ClusterName:          "demo",
	}, time.Millisecond)
	if err == nil {
		t.Fatalf("runStorageProbe() error = nil (outcome %+v), want the API failure reported", outcome)
	}
	if !strings.Contains(err.Error(), "clear stale storage probe") || !strings.Contains(err.Error(), "etcd is unavailable") {
		t.Fatalf("runStorageProbe() error = %v, want it to name the failed cleanup", err)
	}
}

// The residue sweep is what releases a claim that will not finish terminating,
// so the wait that claim exhausted must not starve it (#347).
func TestCleanupStorageProbeSweepsResidueAfterAStuckClaim(t *testing.T) {
	shortenStorageProbeCleanup(t)
	fixture := newStorageProbeStallFixture(t, true)
	if _, err := fixture.clientset.CoreV1().PersistentVolumes().Create(context.Background(), &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-probe"},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: localPathStorageClass,
			ClaimRef:         &corev1.ObjectReference{Namespace: probeNamespace, Name: storageProbePVCName},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), storageProbeCleanupTimeout)
	defer cancel()
	err := cleanupStorageProbe(cleanupCtx, fixture.clientset, fixture.dynamicClient, storageProbeSpec{
		ExpectedStorageClass: localPathStorageClass,
		Engine:               cluster.CSILocalPath,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanupStorageProbe() error = %v, want the stuck claim's deadline", err)
	}
	if _, err := fixture.clientset.CoreV1().PersistentVolumes().Get(context.Background(), "pvc-probe", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("probe PersistentVolume get error = %v, want NotFound: a stuck claim starved the residue sweep", err)
	}
}

// The teardown budget is one budget, not one per step: the waits are capped so
// the sweep still has its slice, and the whole thing stays inside
// storageProbeCleanupTimeout instead of the waits and the sweep each taking it.
func TestStorageProbeWaitContextReservesTheResidueSlice(t *testing.T) {
	shortenStorageProbeCleanup(t)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), storageProbeCleanupTimeout)
	defer cancel()

	waitCtx, waitCancel := storageProbeWaitContext(cleanupCtx)
	defer waitCancel()

	waitDeadline, ok := waitCtx.Deadline()
	if !ok {
		t.Fatal("storageProbeWaitContext() left the waits unbounded, so the sweep can be starved")
	}
	cleanupDeadline, _ := cleanupCtx.Deadline()
	// The reserve is exact by construction; the slack absorbs only the wall
	// clock that moves between the two context constructions.
	const slack = 2 * time.Millisecond
	if remaining := cleanupDeadline.Sub(waitDeadline); remaining < storageProbeResidueTimeout-slack {
		t.Fatalf("storageProbeWaitContext() left the sweep %s, want at least %s", remaining, storageProbeResidueTimeout)
	}
	if waitDeadline.After(cleanupDeadline) {
		t.Fatal("storageProbeWaitContext() escaped the overall cleanup budget")
	}
}

// A teardown must outlive the probe's own deadline — the objects it is deleting
// are exactly what the next pass would otherwise trip over — but must not
// outlive a cancelled lifecycle.
func TestStorageProbeCleanupContextOutlivesADeadlineButNotACancel(t *testing.T) {
	shortenStorageProbeCleanup(t)

	expired, expireCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer expireCancel()
	<-expired.Done()
	cleanupCtx, cleanupCancel := storageProbeCleanupContext(expired)
	defer cleanupCancel()
	if err := cleanupCtx.Err(); err != nil {
		t.Fatalf("cleanup context after an expired probe = %v, want a fresh teardown budget", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancelledCleanup, cancelledCleanupCancel := storageProbeCleanupContext(cancelled)
	defer cancelledCleanupCancel()
	cancel()
	select {
	case <-cancelledCleanup.Done():
	case <-time.After(time.Second):
		t.Fatal("cleanup context survived a cancelled lifecycle, so shutdown cannot stop the sweep")
	}
}

func shortenStorageProbeCleanup(t *testing.T) {
	t.Helper()
	previousCleanup := storageProbeCleanupTimeout
	previousResidue := storageProbeResidueTimeout
	// The residue slice has to be a real fraction of the overall budget, the
	// same way 15s is of 30s: the waits get the remainder and the sweep the
	// rest, so a degenerate pair would leave the sweep nothing to run in.
	storageProbeCleanupTimeout = 60 * time.Millisecond
	storageProbeResidueTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		storageProbeCleanupTimeout = previousCleanup
		storageProbeResidueTimeout = previousResidue
	})
}

// storageProbeStallFixture serves a probe PVC that never finishes terminating:
// from the very first cleanup when stallFromStart is set — the state the next
// pass finds after a teardown outran its bound — or only once the data path has
// verified otherwise.
type storageProbeStallFixture struct {
	dynamicClient *dynamicfake.FakeDynamicClient
	mapper        meta.RESTMapper
	clientset     *kubernetesfake.Clientset
	// applies counts the probe objects that reached the cluster, so a skipped
	// pass can be told from one that ran.
	applies *int
}

func newStorageProbeStallFixture(t *testing.T, stallFromStart bool) storageProbeStallFixture {
	t.Helper()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	reactor := recordApplyReactor(t)
	dynamicClient.PrependReactor("patch", "namespaces", reactor)
	dynamicClient.PrependReactor("patch", "persistentvolumeclaims", reactor)
	dynamicClient.PrependReactor("patch", "pods", reactor)
	applies := 0
	dynamicClient.PrependReactor("patch", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		applies++
		return false, nil, nil
	})

	clientset := kubernetesfake.NewClientset()
	probed := false
	deleted := map[string]bool{}
	clientset.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		claim := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: storageProbePVCName, Namespace: probeNamespace},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: stringPointer(localPathStorageClass)},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}
		// The claim never finishes terminating: the provisioner is behind and
		// cleanup runs out of time.
		if stallFromStart || probed {
			return true, claim, nil
		}
		if deleted["pvc/"+storageProbePVCName] {
			deleted["pvc/"+storageProbePVCName] = false
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, storageProbePVCName)
		}
		return true, claim, nil
	})
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get := action.(k8stesting.GetAction)
		if deleted["pod/"+get.GetName()] {
			deleted["pod/"+get.GetName()] = false
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, get.GetName())
		}
		if get.GetName() == storageProbeReaderPodName {
			probed = true
		}
		return true, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: get.GetName(), Namespace: probeNamespace}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}, nil
	})
	clientset.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleted["pod/"+action.(k8stesting.DeleteAction).GetName()] = true
		return true, nil, nil
	})
	clientset.PrependReactor("delete", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleted["pvc/"+action.(k8stesting.DeleteAction).GetName()] = true
		return true, nil, nil
	})
	return storageProbeStallFixture{dynamicClient: dynamicClient, mapper: storageProbeRESTMapper(), clientset: clientset, applies: &applies}
}

// The probe PVC eventually deletes, but its Longhorn volume can survive it —
// attached and healthy with nothing bound to it, holding disk no object accounts
// for (#347). Cleanup reclaims both the released PersistentVolume and the volume
// behind it, and touches nothing a workload claims.
func TestCleanupStorageProbeReclaimsVolumeResidue(t *testing.T) {
	clientset := kubernetesfake.NewClientset(
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-probe"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName:       longhornStorageClass,
				ClaimRef:               &corev1.ObjectReference{Namespace: probeNamespace, Name: storageProbePVCName},
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: "pvc-probe-handle"}},
			},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-workload"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName: longhornStorageClass,
				ClaimRef:         &corev1.ObjectReference{Namespace: "app", Name: "data"},
			},
		},
	)
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{longhornVolumeResource: "VolumeList"},
		longhornVolumeCR("pvc-probe-handle", "", ""),
		longhornVolumeCR("pvc-detached-probe", probeNamespace, storageProbePVCName),
		longhornVolumeCR("pvc-workload-handle", "app", "data"),
	)

	if err := cleanupStorageProbe(context.Background(), clientset, dynamicClient, storageProbeSpec{
		ExpectedStorageClass: longhornStorageClass,
		Engine:               cluster.CSILonghorn,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := clientset.CoreV1().PersistentVolumes().Get(context.Background(), "pvc-probe", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("probe PersistentVolume get error = %v, want NotFound", err)
	}
	if _, err := clientset.CoreV1().PersistentVolumes().Get(context.Background(), "pvc-workload", metav1.GetOptions{}); err != nil {
		t.Fatalf("workload PersistentVolume was deleted: %v", err)
	}
	volumes := dynamicClient.Resource(longhornVolumeResource).Namespace(longhornNamespace)
	for _, name := range []string{"pvc-probe-handle", "pvc-detached-probe"} {
		if _, err := volumes.Get(context.Background(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("longhorn volume %q get error = %v, want NotFound", name, err)
		}
	}
	if _, err := volumes.Get(context.Background(), "pvc-workload-handle", metav1.GetOptions{}); err != nil {
		t.Fatalf("workload longhorn volume was deleted: %v", err)
	}
}

func longhornVolumeCR(name, claimNamespace, claimName string) *unstructured.Unstructured {
	volume := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "longhorn.io/v1beta2",
		"kind":       "Volume",
		"metadata":   map[string]any{"name": name, "namespace": longhornNamespace},
	}}
	if claimName != "" {
		volume.Object["status"] = map[string]any{
			"kubernetesStatus": map[string]any{"namespace": claimNamespace, "pvcName": claimName},
		}
	}
	return volume
}

func storageProbeRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	return mapper
}

func recordApplyReactor(t *testing.T) func(k8stesting.Action) (bool, runtime.Object, error) {
	t.Helper()
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchAction, ok := action.(interface {
			GetPatchType() types.PatchType
			GetPatch() []byte
		})
		if !ok {
			t.Fatalf("patch action = %T", action)
		}
		if patchAction.GetPatchType() != types.ApplyPatchType {
			t.Fatalf("patch type = %s, want %s", patchAction.GetPatchType(), types.ApplyPatchType)
		}
		var object map[string]any
		if err := json.Unmarshal(patchAction.GetPatch(), &object); err != nil {
			t.Fatalf("decode apply patch: %v", err)
		}
		return true, &unstructured.Unstructured{Object: object}, nil
	}
}

type fakeLoadBalancerReconciler struct {
	calls int
}

func (f *fakeLoadBalancerReconciler) Reconcile(context.Context, cluster.Cluster, []byte) (LoadBalancerResult, error) {
	f.calls++
	return LoadBalancerResult{VIP: "172.30.0.200", Narration: []string{"lb"}}, nil
}

type fakeStorageReconciler struct {
	calls int
}

func (f *fakeStorageReconciler) Reconcile(context.Context, cluster.Cluster, []byte) (StorageResult, error) {
	f.calls++
	return StorageResult{Narration: []string{"storage"}, Phase: StoragePhaseLive, Live: true}, nil
}

func TestFlannelStorageRequiresKubernetesReconciler(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath}
	_, err = Reconcile(context.Background(), Request{
		Cluster: item,
		Client:  &fakeClient{kubeData: []byte("kubeconfig")},
		Observe: func(context.Context) ([]Node, error) {
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured}}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "storage provisioning requires a Kubernetes reconciler") {
		t.Fatalf("Reconcile() error = %v, want missing storage reconciler", err)
	}
}

func TestFlannelReconcileRoutesLonghornStorage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn}
	storage := &fakeStorageReconciler{}
	_, err = Reconcile(context.Background(), Request{
		Cluster: item,
		Client:  &fakeClient{kubeData: []byte("kubeconfig")},
		Storage: storage,
		Observe: func(context.Context) ([]Node, error) {
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if storage.calls != 1 {
		t.Fatalf("storage reconciler calls = %d, want 1 for longhorn", storage.calls)
	}
}

func TestFlannelReconcileRunsStorageAfterLoadBalancer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosVersion = "v1.13.6"
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true, CSI: cluster.CSILocalPath}
	loadBalancer := &fakeLoadBalancerReconciler{}
	storage := &fakeStorageReconciler{}
	result, err := Reconcile(context.Background(), Request{
		Cluster:      item,
		Client:       &fakeClient{kubeData: []byte("kubeconfig")},
		LoadBalancer: loadBalancer,
		Storage:      storage,
		Observe: func(context.Context) ([]Node, error) {
			return []Node{{Name: item.Nodes[0].Name, Role: cluster.RoleControlPlane, IP: item.Nodes[0].IP, Phase: PhaseConfigured}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if loadBalancer.calls != 1 || storage.calls != 1 {
		t.Fatalf("loadBalancer=%d storage=%d, want one each", loadBalancer.calls, storage.calls)
	}
	if got := strings.Join(result.Narration, ","); !strings.Contains(got, "lb") || !strings.Contains(got, "storage") {
		t.Fatalf("narration = %s", got)
	}
	if result.VIP != "172.30.0.200" {
		t.Fatalf("result.VIP = %s", result.VIP)
	}
	if result.StoragePhase != StoragePhaseLive || !result.StorageLive {
		t.Fatalf("storage result = phase %q live %v", result.StoragePhase, result.StorageLive)
	}
}

func TestStorageProbeRendersBarePVCToVerifyDefaultStorageClass(t *testing.T) {
	objects, err := renderStorageProbe(storageProbeSpec{ExpectedStorageClass: localPathStorageClass, ProbeImage: "example.invalid/probe:1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 4 {
		t.Fatalf("renderStorageProbe() objects = %d, want 4", len(objects))
	}
	pvc := objects[1]
	if got, found, err := unstructured.NestedString(pvc.Object, "spec", "storageClassName"); err != nil || found {
		t.Fatalf("storageClassName = %q, found=%v, err=%v; want a bare PVC", got, found, err)
	}
	for _, index := range []int{2, 3} {
		if got, _, err := nestedStringField(objects[index].Object, "spec", "containers", "0", "image"); err != nil || got != "example.invalid/probe:1" {
			t.Fatalf("%s image = %q, %v", objects[index].GetName(), got, err)
		}
	}
}

func TestRecordApplyReactorSmoke(t *testing.T) {
	action := k8stesting.NewPatchAction(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, probeNamespace, "demo", types.ApplyPatchType, []byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"demo","namespace":"talosbox-system"}}`))
	handled, object, err := recordApplyReactor(t)(action)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	unstructuredObject, ok := object.(*unstructured.Unstructured)
	if !ok || unstructuredObject.GetName() != "demo" {
		t.Fatalf("object = %#v", object)
	}
}

func TestStorageProbePodFailureIncludesPodName(t *testing.T) {
	clientset := kubernetesfake.NewClientset()
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get := action.(k8stesting.GetAction)
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: get.GetName(), Namespace: probeNamespace},
			Status:     corev1.PodStatus{Phase: corev1.PodFailed, Message: "boom"},
		}, nil
	})
	err := waitForProbePod(context.Background(), clientset, probeNamespace, storageProbeWriterPodName, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), storageProbeWriterPodName) {
		t.Fatalf("waitForProbePod() error = %v", err)
	}
}

func TestStorageProbePVCBoundIncludesPhase(t *testing.T) {
	clientset := kubernetesfake.NewClientset()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	clientset.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: storageProbePVCName, Namespace: probeNamespace},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		}, nil
	})
	err := waitForBoundPersistentVolumeClaim(ctx, clientset, probeNamespace, storageProbePVCName, localPathStorageClass, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), string(corev1.ClaimPending)) {
		t.Fatalf("waitForBoundPersistentVolumeClaim() error = %v", err)
	}
}

func TestStorageProbeRejectsDifferentDefaultStorageClass(t *testing.T) {
	clientset := kubernetesfake.NewClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: storageProbePVCName, Namespace: probeNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: stringPointer("other-default")},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	})
	err := waitForBoundPersistentVolumeClaim(context.Background(), clientset, probeNamespace, storageProbePVCName, localPathStorageClass, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), `defaulted to StorageClass "other-default"`) {
		t.Fatalf("waitForBoundPersistentVolumeClaim() error = %v", err)
	}
}

func stringPointer(value string) *string { return &value }

func TestStorageProbeDeleteIgnoresMissingObjects(t *testing.T) {
	clientset := kubernetesfake.NewClientset()
	if err := deleteStorageProbePod(context.Background(), clientset, probeNamespace, storageProbeWriterPodName); err != nil {
		t.Fatal(err)
	}
	if err := deleteStorageProbePVC(context.Background(), clientset, probeNamespace, storageProbePVCName); err != nil {
		t.Fatal(err)
	}
}

func TestStorageProbeCleanupWaitsForStaleSucceededPodToDisappear(t *testing.T) {
	clientset := kubernetesfake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: storageProbeWriterPodName, Namespace: probeNamespace},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	})
	gets := 0
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		get := action.(k8stesting.GetAction)
		if gets == 1 {
			return true, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: get.GetName(), Namespace: probeNamespace},
				Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
			}, nil
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, get.GetName())
	})
	if err := deleteStorageProbePodAndWait(context.Background(), clientset, probeNamespace, storageProbeWriterPodName); err != nil {
		t.Fatal(err)
	}
	if gets < 2 {
		t.Fatalf("pod deletion observations = %d, want stale object followed by NotFound", gets)
	}
}

func TestStorageProbeCleanupJoinsDeleteErrors(t *testing.T) {
	clientset := kubernetesfake.NewClientset()
	clientset.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction := action.(k8stesting.DeleteAction)
		return true, nil, fmt.Errorf("delete pod %s", deleteAction.GetName())
	})
	err := cleanupStorageProbe(context.Background(), clientset, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), storageProbeSpec{})
	if err == nil || !strings.Contains(err.Error(), storageProbeWriterPodName) {
		t.Fatalf("cleanupStorageProbe() error = %v", err)
	}
}
