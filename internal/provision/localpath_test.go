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

	if err := runStorageProbe(context.Background(), dynamicClient, mapper, clientset, storageProbeSpec{
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
	err := runStorageProbe(ctx, dynamicClient, mapper, clientset, storageProbeSpec{
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

func TestFlannelReconcileDoesNotRouteLonghornToLocalPath(t *testing.T) {
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
	if storage.calls != 0 {
		t.Fatalf("local-path storage reconciler calls = %d, want 0 for longhorn", storage.calls)
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
	err := cleanupStorageProbe(context.Background(), clientset, storageProbeSpec{})
	if err == nil || !strings.Contains(err.Error(), storageProbeWriterPodName) {
		t.Fatalf("cleanupStorageProbe() error = %v", err)
	}
}
