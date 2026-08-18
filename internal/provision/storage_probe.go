package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/randax/talos-box/internal/cluster"
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
}

// storageProbeCleanupTimeout bounds one teardown attempt. Exceeding it is not a
// failed probe: the objects are already deleting and the next pass collects
// whatever is left. A variable so tests can exercise that bound.
var storageProbeCleanupTimeout = 30 * time.Second

// runStorageProbe writes and reads back through the cluster's default
// StorageClass, then tears its objects down. It returns the advisories the
// caller should report without failing: a teardown that outran its bound is
// one, since the substrate converges behind the verb (#347, following #314).
func runStorageProbe(ctx context.Context, dynamicClient dynamic.Interface, mapper meta.RESTMapper, client kubernetes.Interface, spec storageProbeSpec, interval time.Duration) (warnings []string, err error) {
	cleanup := func() error {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), storageProbeCleanupTimeout)
		defer cleanupCancel()
		return cleanupStorageProbe(cleanupCtx, client, dynamicClient, spec)
	}
	if err := cleanup(); err != nil {
		return nil, fmt.Errorf("clear stale storage probe: %w", err)
	}
	defer func() {
		cleanupErr := cleanup()
		switch {
		case cleanupErr == nil:
		case err == nil && errors.Is(cleanupErr, context.DeadlineExceeded):
			// The data path verified; only the teardown is still running. The
			// daemon reruns this cleanup at the head of its next probe, so
			// failing the verb here would contradict the state it converges to.
			warnings = append(warnings, fmt.Sprintf("storage probe cleanup is still finishing (%v); the daemon keeps reconciling it — watch `tbx status`", cleanupErr))
		case err != nil:
			err = errors.Join(err, fmt.Errorf("cleanup storage probe: %w", cleanupErr))
		default:
			err = fmt.Errorf("cleanup storage probe: %w", cleanupErr)
		}
	}()

	objects, err := renderStorageProbe(spec)
	if err != nil {
		return nil, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, objects[:3]); err != nil {
		return nil, fmt.Errorf("apply storage probe writer: %w", err)
	}
	if err := waitForBoundPersistentVolumeClaim(ctx, client, probeNamespace, storageProbePVCName, spec.ExpectedStorageClass, interval); err != nil {
		return nil, err
	}
	if err := waitForProbePod(ctx, client, probeNamespace, storageProbeWriterPodName, interval); err != nil {
		return nil, err
	}
	if err := deleteStorageProbePod(ctx, client, probeNamespace, storageProbeWriterPodName); err != nil {
		return nil, err
	}
	if err := applyAll(ctx, dynamicClient, mapper, objects[3:]); err != nil {
		return nil, fmt.Errorf("apply storage probe reader: %w", err)
	}
	if err := waitForProbePod(ctx, client, probeNamespace, storageProbeReaderPodName, interval); err != nil {
		return nil, err
	}
	return nil, nil
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
	return poll(ctx, interval, func(ctx context.Context) error {
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
	return poll(ctx, interval, func(ctx context.Context) error {
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

func cleanupStorageProbe(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface, spec storageProbeSpec) error {
	var errs []error
	if err := deleteStorageProbePodAndWait(ctx, client, probeNamespace, storageProbeReaderPodName); err != nil {
		errs = append(errs, err)
	}
	if err := deleteStorageProbePodAndWait(ctx, client, probeNamespace, storageProbeWriterPodName); err != nil {
		errs = append(errs, err)
	}
	if err := deleteStorageProbePVCAndWait(ctx, client, probeNamespace, storageProbePVCName); err != nil {
		errs = append(errs, err)
	}
	// The claim is gone by here, so anything still naming it is residue no
	// other object accounts for — and residue that keeps holding disk.
	if err := deleteStorageProbeVolumeResidue(ctx, client, dynamicClient, spec); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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
	return poll(ctx, 100*time.Millisecond, func(ctx context.Context) error {
		_, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("observe storage probe pod %q deletion: %w", name, err)
		}
		return fmt.Errorf("storage probe pod %q is still terminating", name)
	})
}

func deleteStorageProbePVCAndWait(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	if err := deleteStorageProbePVC(ctx, client, namespace, name); err != nil {
		return err
	}
	return poll(ctx, 100*time.Millisecond, func(ctx context.Context) error {
		_, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("observe storage probe PVC %q deletion: %w", name, err)
		}
		return fmt.Errorf("storage probe PVC %q is still terminating", name)
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
