package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/shellquote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	storageProbePVCName       = "storage-probe"
	storageProbeWriterPodName = "storage-probe-writer"
	storageProbeReaderPodName = "storage-probe-reader"
	storageProbePayload       = "talosbox-storage-probe"
	storageProbeVolumePath    = "/data"
	storageProbeSize          = "1Gi"
)

type storageProbeSpec struct {
	ExpectedStorageClass string
	ProbeImage           string
	// Engine names the curated engine behind the default StorageClass, so
	// cleanup can reclaim the engine-specific residue a deleted claim leaves.
	Engine cluster.CSI
	// ClusterName is only ever reported, never applied: the advisories this
	// probe raises name the cluster whose status the operator should watch.
	ClusterName string
	// Lifecycle is the daemon's own lifetime, and it is what the teardown
	// answers to instead of the probe's context. A teardown must survive the
	// probe spending its budget — the objects are still there either way — but
	// it must not survive the daemon being shut down. A nil Lifecycle (tests,
	// any caller with no lifetime of its own) means the teardown simply detaches
	// from the probe's cancellation.
	Lifecycle context.Context
}

// StorageProbeOutcome reports what one probe pass established.
type StorageProbeOutcome struct {
	// Verified is false when the pass never ran the data path at all: the
	// previous pass's residue was still terminating, so probing on top of it
	// would have measured the teardown rather than the storage engine.
	Verified bool
	// Warnings carry work the daemon keeps finishing behind the verb — never a
	// reason to fail it (#347).
	Warnings []string
}

// storageProbeCleanupTimeout is the whole budget for one teardown attempt —
// the terminating-object waits and the residue sweep together, not each. One
// cleanup() therefore never runs longer than this, and runStorageProbe's two
// cleanups never longer than twice it. Exceeding it is not a failed probe: the
// objects are already deleting and the next pass collects whatever is left. A
// variable so tests can exercise that bound.
var storageProbeCleanupTimeout = 30 * time.Second

// storageProbeResidueTimeout is the slice of storageProbeCleanupTimeout the
// residue sweep is guaranteed, not an extra budget on top of it: the waits are
// capped at the remainder so the sweep still has this much left. The sweep is
// the step that releases a claim which will not finish terminating, so it must
// not be starved by the very wait that claim exhausted — but it also must not
// escape the overall bound or outlive a cancelled lifecycle.
var storageProbeResidueTimeout = 15 * time.Second

// runStorageProbe writes and reads back through the cluster's default
// StorageClass, then tears its objects down. It returns the advisories the
// caller should report without failing: a teardown that outran its bound is
// one, since the substrate converges behind the verb (#347, following #314).
//
// The head cleanup answers to the same rule as the trailing one. A pass whose
// teardown outran its bound leaves a claim that is still terminating, and the
// next pass — the status probe the previous warning sent the operator to
// included — must not fail on residue the daemon already promised to converge.
// It skips its own run instead, so nothing probes on top of a terminating PVC.
func runStorageProbe(ctx context.Context, dynamicClient dynamic.Interface, mapper meta.RESTMapper, client kubernetes.Interface, spec storageProbeSpec, interval time.Duration) (outcome StorageProbeOutcome, err error) {
	cleanup := func() error {
		cleanupCtx, cleanupCancel := storageProbeCleanupContext(ctx, spec.Lifecycle)
		defer cleanupCancel()
		return cleanupStorageProbe(cleanupCtx, client, dynamicClient, spec)
	}
	if cleanupErr := cleanup(); cleanupErr != nil {
		if !onlyCleanupDeadline(cleanupErr) {
			return StorageProbeOutcome{}, fmt.Errorf("clear stale storage probe: %w", cleanupErr)
		}
		return StorageProbeOutcome{Warnings: []string{storageProbeCleanupPendingWarning(spec.ClusterName, cleanupErr)}}, nil
	}
	defer func() {
		cleanupErr := cleanup()
		switch {
		case cleanupErr == nil:
		case err == nil && onlyCleanupDeadline(cleanupErr):
			// The data path verified; only the teardown is still running. The
			// daemon reruns this cleanup at the head of its next probe, so
			// failing the verb here would contradict the state it converges to.
			outcome.Warnings = append(outcome.Warnings, storageProbeCleanupPendingWarning(spec.ClusterName, cleanupErr))
		case err != nil:
			err = errors.Join(err, fmt.Errorf("cleanup storage probe: %w", cleanupErr))
		default:
			err = fmt.Errorf("cleanup storage probe: %w", cleanupErr)
		}
	}()

	objects, err := renderStorageProbe(spec)
	if err != nil {
		return StorageProbeOutcome{}, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, objects[:3]); err != nil {
		return StorageProbeOutcome{}, fmt.Errorf("apply storage probe writer: %w", err)
	}
	if err := waitForBoundPersistentVolumeClaim(ctx, client, probeNamespace, storageProbePVCName, spec.ExpectedStorageClass, interval); err != nil {
		return StorageProbeOutcome{}, err
	}
	if err := waitForProbePod(ctx, client, probeNamespace, storageProbeWriterPodName, interval); err != nil {
		return StorageProbeOutcome{}, err
	}
	if err := deleteStorageProbePod(ctx, client, probeNamespace, storageProbeWriterPodName); err != nil {
		return StorageProbeOutcome{}, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, objects[3:]); err != nil {
		return StorageProbeOutcome{}, fmt.Errorf("apply storage probe reader: %w", err)
	}
	if err := waitForProbePod(ctx, client, probeNamespace, storageProbeReaderPodName, interval); err != nil {
		return StorageProbeOutcome{}, err
	}
	return StorageProbeOutcome{Verified: true}, nil
}

// storageProbeCleanupPendingWarning names the one state a probe cleanup can be
// left in without anything being wrong: still running. The cluster is named
// because that is what `tbx status` needs, and quoted because the line invites
// a paste (SPEC §10).
func storageProbeCleanupPendingWarning(clusterName string, err error) string {
	hint := "watch it with: tbx status"
	if clusterName != "" {
		hint += " " + shellquote.Quote(clusterName)
	}
	return fmt.Sprintf("storage probe cleanup is still finishing (%v); the daemon keeps reconciling it — %s", err, hint)
}

// errStorageProbeTerminating marks the benign half of a cleanup wait's outcome:
// the object is deleting and has not gone yet. The polling helper joins that
// last observation with the deadline it finally hits, so it carries no more
// information than the deadline does.
var errStorageProbeTerminating = errors.New("still terminating")

// onlyCleanupDeadline reports whether the cleanup budget running out is the
// *sole* cause of err. A deadline joined with a real API error is not a
// teardown that merely needs more time: something else failed, and downgrading
// that to an advisory would hide it.
func onlyCleanupDeadline(err error) bool {
	return err != nil && errors.Is(err, context.DeadlineExceeded) && cleanupCausesAreBenign(err)
}

func cleanupCausesAreBenign(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if !cleanupCausesAreBenign(cause) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return cleanupCausesAreBenign(wrapped)
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errStorageProbeTerminating)
}

func renderStorageProbe(spec storageProbeSpec) ([]unstructured.Unstructured, error) {
	if spec.ExpectedStorageClass == "" {
		return nil, errors.New("storage probe requires an expected default StorageClass")
	}
	if spec.ProbeImage == "" {
		return nil, errors.New("storage probe requires a probe image")
	}
	objects, err := decodeObjects([]byte(fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    talosbox.dev/managed: "true"
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
  labels:
    talosbox.dev/managed: "true"
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: %s
---
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    talosbox.dev/managed: "true"
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: %s
      command:
        - sh
        - -c
        - printf '%s' > %s/probe && sync
      volumeMounts:
        - name: storage
          mountPath: %s
  volumes:
    - name: storage
      persistentVolumeClaim:
        claimName: %s
---
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    talosbox.dev/managed: "true"
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: %s
      command:
        - sh
        - -c
        - test "$(cat %s/probe)" = "%s"
      volumeMounts:
        - name: storage
          mountPath: %s
  volumes:
    - name: storage
      persistentVolumeClaim:
        claimName: %s
`, probeNamespace, storageProbePVCName, probeNamespace, storageProbeSize, storageProbeWriterPodName, probeNamespace, spec.ProbeImage, storageProbePayload, storageProbeVolumePath, storageProbeVolumePath, storageProbePVCName, storageProbeReaderPodName, probeNamespace, spec.ProbeImage, storageProbeVolumePath, storageProbePayload, storageProbeVolumePath, storageProbePVCName)))
	if err != nil {
		return nil, fmt.Errorf("decode storage probe: %w", err)
	}
	return objects, nil
}

func waitForBoundPersistentVolumeClaim(ctx context.Context, client kubernetes.Interface, namespace, name, expectedStorageClass string, interval time.Duration) error {
	return poll(ctx, GateStorageProbePVC, interval, func(ctx context.Context) error {
		persistentVolumeClaim, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if persistentVolumeClaim.Status.Phase != corev1.ClaimBound {
			return fmt.Errorf("storage probe PVC %q phase = %s", name, persistentVolumeClaim.Status.Phase)
		}
		if persistentVolumeClaim.Spec.StorageClassName == nil || *persistentVolumeClaim.Spec.StorageClassName != expectedStorageClass {
			actual := ""
			if persistentVolumeClaim.Spec.StorageClassName != nil {
				actual = *persistentVolumeClaim.Spec.StorageClassName
			}
			return terminal(fmt.Errorf("storage probe PVC %q defaulted to StorageClass %q, want %q", name, actual, expectedStorageClass))
		}
		return nil
	})
}

func waitForProbePod(ctx context.Context, client kubernetes.Interface, namespace, name string, interval time.Duration) error {
	return poll(ctx, GateStorageProbePod, interval, func(ctx context.Context) error {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			return nil
		case corev1.PodFailed:
			if pod.Status.Message != "" {
				return terminal(fmt.Errorf("storage probe pod %q failed: %s", name, pod.Status.Message))
			}
			return terminal(fmt.Errorf("storage probe pod %q failed", name))
		default:
			return fmt.Errorf("storage probe pod %q phase = %s", name, pod.Status.Phase)
		}
	})
}

// storageProbeCleanupContext gives one teardown attempt its own
// storageProbeCleanupTimeout on a base detached from ctx, then hangs two
// watchers off it. Three rows, one per way the probe can end:
//
//   - ctx hit its DEADLINE: the teardown continues on its fresh budget. The
//     probe spent its own time, but the objects it created still have to go or
//     the next pass finds residue it must skip over (#347).
//   - ctx was CANCELLED — a newer reconcile superseded this one, the storage
//     phase was invalidated, or a destroy/drain is tearing the cluster down:
//     the teardown dies promptly. Its objects have fixed names, so a superseded
//     teardown would otherwise delete the very probe objects the superseding
//     pass is creating, and drain/destroy would wait out the whole budget for a
//     teardown nobody is waiting on.
//   - lifecycle cancelled (daemon shutting down): the teardown dies promptly,
//     whichever way the probe itself ended.
//
// The probe watcher fires for both endings and cancels only for the cancelled
// one, so a latched deadline makes it a no-op — which is right, because the
// lifecycle watcher still covers a shutdown that arrives afterwards. Without a
// lifecycle the teardown answers to its own timeout and to ctx's cancellation,
// which is the most a caller with no lifetime to offer can be given.
func storageProbeCleanupContext(ctx context.Context, lifecycle context.Context) (context.Context, context.CancelFunc) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storageProbeCleanupTimeout)
	stops := []func() bool{context.AfterFunc(ctx, func() {
		if storageProbeContextWasCancelled(ctx) {
			cancel()
		}
	})}
	if lifecycle != nil {
		stops = append(stops, context.AfterFunc(lifecycle, cancel))
	}
	return cleanupCtx, func() {
		for _, stop := range stops {
			stop()
		}
		cancel()
	}
}

// storageProbeContextWasCancelled tells the cancelled ending from the expired
// one. Err is the reading that separates them — Canceled for either kind of
// cancel, DeadlineExceeded for a latched deadline — and Cause is consulted as
// well so a cause that only wraps Canceled still reads as cancelled.
func storageProbeContextWasCancelled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled)
}

func cleanupStorageProbe(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface, spec storageProbeSpec) error {
	// The terminating-object waits get the cleanup budget minus the residue
	// sweep's guaranteed slice. Anything still naming the claim is residue no
	// other object accounts for — and residue that keeps holding disk — so a
	// claim that will not finish terminating, the case where releasing its
	// volume matters most, must not be the case that starves the sweep. Capping
	// the waits rather than detaching the sweep keeps the whole teardown inside
	// one budget and inside one cancellation.
	waitCtx, waitCancel := storageProbeWaitContext(ctx)
	defer waitCancel()
	var errs []error
	if err := deleteStorageProbePodAndWait(waitCtx, client, probeNamespace, storageProbeReaderPodName); err != nil {
		errs = append(errs, err)
	}
	if err := deleteStorageProbePodAndWait(waitCtx, client, probeNamespace, storageProbeWriterPodName); err != nil {
		errs = append(errs, err)
	}
	if err := deleteStorageProbePVCAndWait(waitCtx, client, probeNamespace, storageProbePVCName); err != nil {
		errs = append(errs, err)
	}
	if err := deleteStorageProbeVolumeResidue(ctx, client, dynamicClient, spec); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// storageProbeWaitContext caps the terminating-object waits so the residue
// sweep inherits at least storageProbeResidueTimeout of the same budget. A
// cleanup context with no deadline, or one already down to less than the
// sweep's slice, has nothing to reserve and the waits simply share what is
// left — best effort is the most the bound can offer there.
func storageProbeWaitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	budget := time.Until(deadline) - storageProbeResidueTimeout
	if budget <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, budget)
}

// deleteStorageProbeVolumeResidue reclaims what a deleted probe claim can leave
// behind: a Released PersistentVolume whose provisioner never caught up, and —
// on Longhorn — a volume that stays attached and healthy with nothing bound to
// it (#347). Both name the probe claim, so neither can belong to a workload.
func deleteStorageProbeVolumeResidue(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface, spec storageProbeSpec) error {
	list, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list storage probe persistent volumes: %w", err)
	}
	var errs []error
	handles := make(map[string]bool)
	for i := range list.Items {
		persistentVolume := &list.Items[i]
		if !storageProbePVResidue(persistentVolume) {
			continue
		}
		if persistentVolume.Spec.CSI != nil {
			handles[persistentVolume.Spec.CSI.VolumeHandle] = true
		}
		if err := client.CoreV1().PersistentVolumes().Delete(ctx, persistentVolume.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete storage probe PersistentVolume %q: %w", persistentVolume.Name, err))
		}
	}
	if spec.Engine == cluster.CSILonghorn {
		if err := deleteLonghornProbeVolumes(ctx, dynamicClient, handles); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deleteLonghornProbeVolumes removes the probe's Longhorn volumes, whether they
// are still reachable through a PersistentVolume handle or only through the
// claim reference Longhorn records on the volume itself.
func deleteLonghornProbeVolumes(ctx context.Context, client dynamic.Interface, handles map[string]bool) error {
	volumes := client.Resource(longhornVolumeResource).Namespace(longhornNamespace)
	list, err := volumes.List(ctx, metav1.ListOptions{})
	if err != nil {
		// An absent CRD means Longhorn is not installed, and so holds nothing.
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list longhorn volumes for storage probe cleanup: %w", err)
	}
	var errs []error
	for i := range list.Items {
		volume := &list.Items[i]
		if !handles[volume.GetName()] && !longhornVolumeClaimsStorageProbe(volume) {
			continue
		}
		if err := volumes.Delete(ctx, volume.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete storage probe longhorn volume %q: %w", volume.GetName(), err))
		}
	}
	return errors.Join(errs...)
}

func longhornVolumeClaimsStorageProbe(volume *unstructured.Unstructured) bool {
	return nestedString(volume, "status", "kubernetesStatus", "namespace") == probeNamespace &&
		nestedString(volume, "status", "kubernetesStatus", "pvcName") == storageProbePVCName
}

func deleteStorageProbePodAndWait(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	if err := deleteStorageProbePod(ctx, client, namespace, name); err != nil {
		return err
	}
	return poll(ctx, GateStorageProbeCleanup, 100*time.Millisecond, func(ctx context.Context) error {
		_, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("observe storage probe pod %q deletion: %w", name, err)
		}
		return fmt.Errorf("storage probe pod %q is %w", name, errStorageProbeTerminating)
	})
}

func deleteStorageProbePVCAndWait(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	if err := deleteStorageProbePVC(ctx, client, namespace, name); err != nil {
		return err
	}
	return poll(ctx, GateStorageProbeCleanup, 100*time.Millisecond, func(ctx context.Context) error {
		_, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("observe storage probe PVC %q deletion: %w", name, err)
		}
		return fmt.Errorf("storage probe PVC %q is %w", name, errStorageProbeTerminating)
	})
}

func deleteStorageProbePod(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	err := client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete storage probe pod %q: %w", name, err)
	}
	return nil
}

func deleteStorageProbePVC(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	err := client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete storage probe PVC %q: %w", name, err)
	}
	return nil
}
