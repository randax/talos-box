package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

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
}

func runStorageProbe(ctx context.Context, dynamicClient dynamic.Interface, mapper meta.RESTMapper, client kubernetes.Interface, spec storageProbeSpec, interval time.Duration) (err error) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := cleanupStorageProbe(cleanupCtx, client, spec); err != nil {
		cleanupCancel()
		return fmt.Errorf("clear stale storage probe: %w", err)
	}
	cleanupCancel()
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanupErr := cleanupStorageProbe(cleanupCtx, client, spec)
		cleanupCancel()
		if cleanupErr != nil {
			if err != nil {
				err = errors.Join(err, fmt.Errorf("cleanup storage probe: %w", cleanupErr))
			} else {
				err = fmt.Errorf("cleanup storage probe: %w", cleanupErr)
			}
		}
	}()

	objects, err := renderStorageProbe(spec)
	if err != nil {
		return err
	}
	if err := applyAll(ctx, dynamicClient, mapper, objects[:3]); err != nil {
		return fmt.Errorf("apply storage probe writer: %w", err)
	}
	if err := waitForBoundPersistentVolumeClaim(ctx, client, probeNamespace, storageProbePVCName, spec.ExpectedStorageClass, interval); err != nil {
		return err
	}
	if err := waitForProbePod(ctx, client, probeNamespace, storageProbeWriterPodName, interval); err != nil {
		return err
	}
	if err := deleteStorageProbePod(ctx, client, probeNamespace, storageProbeWriterPodName); err != nil {
		return err
	}
	if err := applyAll(ctx, dynamicClient, mapper, objects[3:]); err != nil {
		return fmt.Errorf("apply storage probe reader: %w", err)
	}
	if err := waitForProbePod(ctx, client, probeNamespace, storageProbeReaderPodName, interval); err != nil {
		return err
	}
	return nil
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

func cleanupStorageProbe(ctx context.Context, client kubernetes.Interface, _ storageProbeSpec) error {
	var errs []error
	if err := deleteStorageProbePod(ctx, client, probeNamespace, storageProbeReaderPodName); err != nil {
		errs = append(errs, err)
	}
	if err := deleteStorageProbePod(ctx, client, probeNamespace, storageProbeWriterPodName); err != nil {
		errs = append(errs, err)
	}
	if err := deleteStorageProbePVC(ctx, client, probeNamespace, storageProbePVCName); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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
