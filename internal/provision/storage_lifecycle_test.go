package provision

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCountProvisionedStorageVolumesCountsOnlyMatchingEnginePVs(t *testing.T) {
	client := kubernetesfake.NewClientset(
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "longhorn-user"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName: longhornStorageClass,
				ClaimRef:         &corev1.ObjectReference{Namespace: "app", Name: "data"},
			},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "probe-residue"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName: longhornStorageClass,
				ClaimRef:         &corev1.ObjectReference{Namespace: probeNamespace, Name: storageProbePVCName},
			},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "other-engine"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName: localPathStorageClass,
				ClaimRef:         &corev1.ObjectReference{Namespace: "app", Name: "cache"},
			},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "no-class"},
			Spec:       corev1.PersistentVolumeSpec{},
		},
	)

	count, err := countProvisionedStorageVolumes(context.Background(), client, cluster.CSILonghorn)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("countProvisionedStorageVolumes() = %d, want 1", count)
	}
}

// The csi gate refuses on these names, so they have to be the ones an operator
// can act on: the claim, sorted, and the volume itself where the claim
// reference is already gone (#393).
func TestProvisionedStorageVolumeClaimsNamesTheClaims(t *testing.T) {
	client := kubernetesfake.NewClientset(
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-logs"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName: longhornStorageClass,
				ClaimRef:         &corev1.ObjectReference{Namespace: "app", Name: "logs"},
			},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-data"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName: longhornStorageClass,
				ClaimRef:         &corev1.ObjectReference{Namespace: "app", Name: "data"},
			},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-released"},
			Spec:       corev1.PersistentVolumeSpec{StorageClassName: longhornStorageClass},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "probe-residue"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName: longhornStorageClass,
				ClaimRef:         &corev1.ObjectReference{Namespace: probeNamespace, Name: storageProbePVCName},
			},
		},
	)

	claims, err := provisionedStorageVolumeClaims(context.Background(), client, cluster.CSILonghorn)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app/data", "app/logs", "pvc-released"}
	if !slices.Equal(claims, want) {
		t.Fatalf("provisionedStorageVolumeClaims() = %v, want %v", claims, want)
	}
}

func TestCountProvisionedStorageVolumesReturnsListErrors(t *testing.T) {
	client := kubernetesfake.NewClientset()
	client.PrependReactor("list", "persistentvolumes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp timeout")
	})

	_, err := countProvisionedStorageVolumes(context.Background(), client, cluster.CSILocalPath)
	if err == nil || !strings.Contains(err.Error(), "dial tcp timeout") {
		t.Fatalf("countProvisionedStorageVolumes() error = %v, want list failure", err)
	}
}

func TestCountProvisionedStorageVolumesRejectsUnknownEngine(t *testing.T) {
	_, err := countProvisionedStorageVolumes(context.Background(), kubernetesfake.NewClientset(), cluster.CSI("rook"))
	if err == nil || !strings.Contains(err.Error(), `unsupported storage engine "rook"`) {
		t.Fatalf("countProvisionedStorageVolumes() error = %v", err)
	}
}

func TestDeleteStorageEngineObjectsRemovesOwnedObjectsForBothEngines(t *testing.T) {
	for _, engine := range []cluster.CSI{cluster.CSILocalPath, cluster.CSILonghorn} {
		t.Run(string(engine), func(t *testing.T) {
			objects := storageLifecycleObjects(t, engine)
			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			mapper := storageLifecycleTestMapper(objects)
			createStorageLifecycleObjects(t, client, mapper, objects)

			if err := deleteStorageEngineObjects(context.Background(), client, mapper, engine); err != nil {
				t.Fatal(err)
			}

			deletes := storageLifecycleDeleteActions(client.Actions())
			if len(deletes) == 0 {
				t.Fatal("deleteStorageEngineObjects() issued no deletes")
			}
			last := deletes[len(deletes)-1]
			if last.GetResource().Resource != "namespaces" {
				t.Fatalf("last delete resource = %s, want namespaces", last.GetResource().Resource)
			}

			for i := range objects {
				object := objects[i]
				if storageLifecycleSkipDelete(object) {
					continue
				}
				_, err := storageLifecycleTestResource(t, client, mapper, &object).Get(context.Background(), object.GetName(), metav1.GetOptions{})
				if !apierrors.IsNotFound(err) {
					t.Fatalf("%s %q get error = %v, want NotFound", object.GetKind(), object.GetName(), err)
				}
			}

			if err := deleteStorageEngineObjects(context.Background(), client, mapper, engine); err != nil {
				t.Fatalf("idempotent rerun error = %v", err)
			}
		})
	}
}

func TestDeleteStorageEngineObjectsRejectsUnownedCollisionBeforeDeletingOwned(t *testing.T) {
	objects := storageLifecycleObjects(t, cluster.CSILocalPath)
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := storageLifecycleTestMapper(objects)
	createStorageLifecycleObjects(t, client, mapper, objects)

	ordered := storageDeletionOrder(objects)
	owned := firstStorageLifecycleObject(t, ordered, func(object unstructured.Unstructured) bool {
		return !storageLifecycleSkipDelete(object) && object.GetKind() != "Namespace"
	})
	unowned := firstStorageLifecycleObject(t, ordered, func(object unstructured.Unstructured) bool {
		return !storageLifecycleSkipDelete(object) && object.GetKind() != "Namespace" && object.GetName() != owned.GetName()
	})
	unownedLive, err := storageLifecycleTestResource(t, client, mapper, unowned).Get(context.Background(), unowned.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	unownedLive.SetLabels(nil)
	unownedLive.SetManagedFields(nil)
	if _, err := storageLifecycleTestResource(t, client, mapper, unowned).Update(context.Background(), unownedLive, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	err = deleteStorageEngineObjects(context.Background(), client, mapper, cluster.CSILocalPath)
	if err == nil || !strings.Contains(err.Error(), "unmanaged storage") {
		t.Fatalf("deleteStorageEngineObjects() error = %v, want unmanaged storage", err)
	}
	if _, err := storageLifecycleTestResource(t, client, mapper, owned).Get(context.Background(), owned.GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("owned object was deleted before validation completed: %v", err)
	}
}

func TestDeleteRenderedStorageObjectsHonorsManagedFieldsOwnership(t *testing.T) {
	objects := storageLifecycleObjects(t, cluster.CSILocalPath)
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := storageLifecycleTestMapper(objects)
	createStorageLifecycleObjects(t, client, mapper, objects)

	managedByFields := firstStorageLifecycleObject(t, storageDeletionOrder(objects), func(object unstructured.Unstructured) bool {
		return !storageLifecycleSkipDelete(object) && object.GetKind() != "Namespace"
	})
	live, err := storageLifecycleTestResource(t, client, mapper, managedByFields).Get(context.Background(), managedByFields.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	live.SetLabels(nil)
	live.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: fieldManager}})
	if _, err := storageLifecycleTestResource(t, client, mapper, managedByFields).Update(context.Background(), live, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := deleteRenderedStorageObjects(context.Background(), client, mapper, objects); err != nil {
		t.Fatal(err)
	}
	if _, err := storageLifecycleTestResource(t, client, mapper, managedByFields).Get(context.Background(), managedByFields.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("managedFields-owned object get error = %v, want NotFound", err)
	}
}

func TestDeleteRenderedStorageObjectsReturnsLookupErrors(t *testing.T) {
	objects := storageLifecycleObjects(t, cluster.CSILocalPath)
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := storageLifecycleTestMapper(objects)
	createStorageLifecycleObjects(t, client, mapper, objects)

	failingObject := firstStorageLifecycleObject(t, storageDeletionOrder(objects), func(object unstructured.Unstructured) bool {
		return !storageLifecycleSkipDelete(object) && object.GetKind() != "Namespace"
	})
	resourceName := storageLifecycleResourceName(t, mapper, failingObject)
	client.PrependReactor("get", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		if getAction.GetResource().Resource == resourceName &&
			getAction.GetNamespace() == failingObject.GetNamespace() &&
			getAction.GetName() == failingObject.GetName() {
			return true, nil, errors.New("dial tcp timeout")
		}
		return false, nil, nil
	})

	err := deleteRenderedStorageObjects(context.Background(), client, mapper, objects)
	if err == nil || !strings.Contains(err.Error(), "dial tcp timeout") {
		t.Fatalf("deleteRenderedStorageObjects() error = %v, want lookup failure", err)
	}
}

func TestDeleteRenderedStorageObjectsNeverDeletesPersistentVolumesOrClaims(t *testing.T) {
	objects, err := decodeObjects([]byte(`apiVersion: v1
kind: PersistentVolume
metadata:
  name: retained-pv
  labels:
    talosbox.dev/managed: "true"
spec:
  capacity:
    storage: 1Gi
  accessModes:
    - ReadWriteOnce
  hostPath:
    path: /tmp/retained
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: retained-pvc
  namespace: demo
  labels:
    talosbox.dev/managed: "true"
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Namespace
metadata:
  name: demo
  labels:
    talosbox.dev/managed: "true"
`))
	if err != nil {
		t.Fatal(err)
	}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := storageLifecycleTestMapper(objects)
	createStorageLifecycleObjects(t, client, mapper, objects)

	if err := deleteRenderedStorageObjects(context.Background(), client, mapper, objects); err != nil {
		t.Fatal(err)
	}

	deletes := storageLifecycleDeleteActions(client.Actions())
	if len(deletes) != 1 || deletes[0].GetResource().Resource != "namespaces" {
		t.Fatalf("delete actions = %v, want only namespace delete", deletes)
	}
	if _, err := storageLifecycleTestResource(t, client, mapper, &objects[0]).Get(context.Background(), objects[0].GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("PersistentVolume was deleted: %v", err)
	}
	if _, err := storageLifecycleTestResource(t, client, mapper, &objects[1]).Get(context.Background(), objects[1].GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("PersistentVolumeClaim was deleted: %v", err)
	}
}

func TestReplaceDriftedStorageClassDeletesOnlyOnImmutableParameterDrift(t *testing.T) {
	rendered := storageClassFixture("longhorn", map[string]any{"numberOfReplicas": "2"}, true)
	tests := []struct {
		name       string
		live       *unstructured.Unstructured
		wantDelete bool
		wantErr    string
	}{
		{name: "no live StorageClass", live: nil, wantDelete: false},
		{name: "same parameters", live: storageClassFixture("longhorn", map[string]any{"numberOfReplicas": "2"}, true), wantDelete: false},
		{name: "drifted parameters", live: storageClassFixture("longhorn", map[string]any{"numberOfReplicas": "3"}, true), wantDelete: true},
		{
			name:       "drifted but unowned StorageClass",
			live:       storageClassFixture("longhorn", map[string]any{"numberOfReplicas": "3"}, false),
			wantDelete: false,
			wantErr:    "not managed by talosbox",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []unstructured.Unstructured{*rendered}
			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			mapper := storageLifecycleTestMapper(objects)
			if tt.live != nil {
				createStorageLifecycleObjects(t, client, mapper, []unstructured.Unstructured{*tt.live})
			}

			err := replaceDriftedStorageClass(context.Background(), client, mapper, objects, time.Millisecond)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("replaceDriftedStorageClass() error = %v, want containing %q", err, tt.wantErr)
			}

			gotDelete := len(storageLifecycleDeleteActions(client.Actions())) > 0
			if gotDelete != tt.wantDelete {
				t.Fatalf("StorageClass delete issued = %v, want %v", gotDelete, tt.wantDelete)
			}
		})
	}
}

func TestReplaceDriftedStorageClassWaitsOutAsynchronousDeletion(t *testing.T) {
	rendered := storageClassFixture("longhorn", map[string]any{"numberOfReplicas": "2"}, true)
	live := storageClassFixture("longhorn", map[string]any{"numberOfReplicas": "3"}, true)
	objects := []unstructured.Unstructured{*rendered}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := storageLifecycleTestMapper(objects)

	// Serve the drift-inspection get, then keep the object visible for two
	// more gets after the delete — Kubernetes deletion is asynchronous, so a
	// terminating StorageClass must not be treated as gone.
	gets := 0
	client.PrependReactor("get", "storageclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets <= 3 {
			return true, live.DeepCopy(), nil
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, "longhorn")
	})

	if err := replaceDriftedStorageClass(context.Background(), client, mapper, objects, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if len(storageLifecycleDeleteActions(client.Actions())) == 0 {
		t.Fatal("replaceDriftedStorageClass() issued no delete")
	}
	if gets < 4 {
		t.Fatalf("replaceDriftedStorageClass() returned after %d gets, before deletion completed", gets)
	}
}

// longhorn-driver-deployer re-creates the `longhorn` StorageClass from the
// longhorn-storageclass ConfigMap, and clusters provisioned before #338 hold a
// definition without the managed label. The class is still tbx-installed, so
// ownership falls back to matching the live class against the rendered legacy
// shape — provisioner, reclaim policy, and every parameter except the
// topology-derived numberOfReplicas. A user's own longhorn-named class with
// customized fields must never match.
func TestValidateRenderedStorageObjectsAdoptsUnlabeledEngineStorageClass(t *testing.T) {
	tests := []struct {
		name          string
		provisioner   string
		parameters    map[string]any
		reclaimPolicy string
		wantErr       bool
	}{
		{name: "longhorn re-created its own class", provisioner: longhornProvisioner,
			parameters: map[string]any{"numberOfReplicas": "3", "staleReplicaTimeout": "30"}},
		{name: "replica-count drift alone is still tbx's own class", provisioner: longhornProvisioner,
			parameters: map[string]any{"numberOfReplicas": "1", "staleReplicaTimeout": "30"}},
		{name: "foreign provisioner stays user-owned", provisioner: "csi.example.com",
			parameters: map[string]any{"numberOfReplicas": "3", "staleReplicaTimeout": "30"}, wantErr: true},
		{name: "customized parameters stay user-owned", provisioner: longhornProvisioner,
			parameters: map[string]any{"numberOfReplicas": "3", "staleReplicaTimeout": "600"}, wantErr: true},
		{name: "extra parameter stays user-owned", provisioner: longhornProvisioner,
			parameters: map[string]any{"numberOfReplicas": "3", "staleReplicaTimeout": "30", "dataLocality": "best-effort"}, wantErr: true},
		{name: "customized reclaim policy stays user-owned", provisioner: longhornProvisioner,
			parameters:    map[string]any{"numberOfReplicas": "3", "staleReplicaTimeout": "30"},
			reclaimPolicy: "Retain", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered := storageClassFixture(longhornStorageClass, map[string]any{"numberOfReplicas": "3", "staleReplicaTimeout": "30"}, true)
			rendered.Object["provisioner"] = longhornProvisioner
			rendered.Object["reclaimPolicy"] = "Delete"
			live := storageClassFixture(longhornStorageClass, tt.parameters, false)
			live.Object["provisioner"] = tt.provisioner
			live.Object["reclaimPolicy"] = "Delete"
			if tt.reclaimPolicy != "" {
				live.Object["reclaimPolicy"] = tt.reclaimPolicy
			}
			objects := []unstructured.Unstructured{*rendered}
			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			mapper := storageLifecycleTestMapper(objects)
			createStorageLifecycleObjects(t, client, mapper, []unstructured.Unstructured{*live})

			err := validateRenderedStorageObjects(context.Background(), client, mapper, objects)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "unmanaged storage") {
					t.Fatalf("validateRenderedStorageObjects() error = %v, want unmanaged storage", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateRenderedStorageObjects() error = %v, want the re-created class adopted", err)
			}
		})
	}
}

func storageClassFixture(name string, parameters map[string]any, owned bool) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1",
		"kind":       "StorageClass",
		"metadata":   map[string]any{"name": name},
		"parameters": parameters,
	}}
	if owned {
		ensureManagedLabel(object)
	}
	return object
}

func TestDeleteRenderedStorageObjectsRecoversFromInterruptedRun(t *testing.T) {
	objects := storageLifecycleObjects(t, cluster.CSILocalPath)
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := storageLifecycleTestMapper(objects)
	createStorageLifecycleObjects(t, client, mapper, objects)

	ordered := storageDeletionOrder(objects)
	failCandidate := firstStorageLifecycleObject(t, ordered, func(object unstructured.Unstructured) bool {
		return !storageLifecycleSkipDelete(object) && object.GetKind() != "Namespace"
	})
	failed := false
	client.PrependReactor("delete", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction, ok := action.(k8stesting.DeleteAction)
		if !ok {
			return false, nil, nil
		}
		if !failed &&
			deleteAction.GetName() == failCandidate.GetName() &&
			deleteAction.GetNamespace() == failCandidate.GetNamespace() &&
			deleteAction.GetResource().Resource == storageLifecycleResourceName(t, mapper, failCandidate) {
			failed = true
			return true, nil, errors.New("simulated interruption")
		}
		return false, nil, nil
	})

	err := deleteRenderedStorageObjects(context.Background(), client, mapper, objects)
	if err == nil || !strings.Contains(err.Error(), "simulated interruption") {
		t.Fatalf("first deleteRenderedStorageObjects() error = %v", err)
	}
	if err := deleteRenderedStorageObjects(context.Background(), client, mapper, objects); err != nil {
		t.Fatalf("rerun deleteRenderedStorageObjects() error = %v", err)
	}
	for i := range objects {
		object := objects[i]
		if storageLifecycleSkipDelete(object) {
			continue
		}
		_, err := storageLifecycleTestResource(t, client, mapper, &object).Get(context.Background(), object.GetName(), metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("%s %q get error after rerun = %v, want NotFound", object.GetKind(), object.GetName(), err)
		}
	}
}

// Longhorn's admission webhooks fail closed: once their Service is gone they
// reject every PVC bind in the cluster and hold longhorn-system in Terminating
// (#386). Teardown therefore has to take the cluster-wide objects down first —
// webhook configurations and StorageClasses, the latter so no class advertises
// an engine the cluster no longer runs (#394).
func TestStorageDeletionOrderTakesClusterWideObjectsDownFirst(t *testing.T) {
	objects := storageLifecycleObjects(t, cluster.CSILonghorn)
	ordered := storageDeletionOrder(objects)

	names := map[string]int{}
	for i, object := range ordered {
		names[object.GetKind()+"/"+object.GetName()] = i
	}
	service, ok := names["Service/"+longhornAdmissionWebhookService]
	if !ok {
		t.Fatalf("rendered Longhorn objects do not contain the %q Service", longhornAdmissionWebhookService)
	}
	manager, ok := names["DaemonSet/"+longhornManagerName]
	if !ok {
		t.Fatalf("rendered Longhorn objects do not contain the %q DaemonSet", longhornManagerName)
	}
	for _, key := range []string{
		"ValidatingWebhookConfiguration/" + longhornValidatingWebhookName,
		"MutatingWebhookConfiguration/" + longhornMutatingWebhookName,
		"StorageClass/" + longhornStaticStorageClass,
		"StorageClass/" + longhornStorageClass,
		"CSIDriver/" + longhornProvisioner,
	} {
		position, ok := names[key]
		if !ok {
			t.Fatalf("storage deletion order is missing %s", key)
		}
		if position > service {
			t.Fatalf("%s is deleted at %d, after the webhook Service at %d", key, position, service)
		}
		// The controllers go first or they simply reinstall what this move
		// deletes (#338).
		if position < manager {
			t.Fatalf("%s is deleted at %d, before the manager DaemonSet at %d", key, position, manager)
		}
	}
	last := ordered[len(ordered)-1]
	if last.GetKind() != "Namespace" {
		t.Fatalf("last deletion = %s/%s, want the Namespace", last.GetKind(), last.GetName())
	}
}

// Adopting longhorn-manager's own cluster-scoped objects must not adopt a
// user's object that merely shares the name: the guard reads identity, and
// anything else stays refused.
func TestLonghornOwnedRuntimeObjectReadsIdentityNotName(t *testing.T) {
	tests := []struct {
		name  string
		live  *unstructured.Unstructured
		owned bool
	}{
		{
			name:  "webhook configuration served by longhorn",
			live:  longhornRuntimeTestObject(t, "ValidatingWebhookConfiguration", longhornValidatingWebhookName, longhornNamespace),
			owned: true,
		},
		{
			name: "webhook configuration served from elsewhere",
			live: longhornRuntimeTestObject(t, "MutatingWebhookConfiguration", longhornMutatingWebhookName, "team-admission"),
		},
		{
			name: "webhook configuration with no webhooks at all",
			live: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "admissionregistration.k8s.io/v1",
				"kind":       "ValidatingWebhookConfiguration",
				"metadata":   map[string]any{"name": longhornValidatingWebhookName},
			}},
		},
		{
			name:  "static class provisioned by longhorn",
			live:  storageLifecycleLiveForm(&longhornRuntimeObjects()[2]),
			owned: true,
		},
		{
			name:  "csi driver registration for longhorn",
			live:  storageLifecycleLiveForm(&longhornRuntimeObjects()[3]),
			owned: true,
		},
		{
			name: "csi driver registration for another driver",
			live: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "storage.k8s.io/v1",
				"kind":       "CSIDriver",
				"metadata":   map[string]any{"name": "csi.example.com"},
			}},
		},
		{
			name: "static class provisioned by something else",
			live: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion":  "storage.k8s.io/v1",
				"kind":        "StorageClass",
				"metadata":    map[string]any{"name": longhornStaticStorageClass},
				"provisioner": "rancher.io/local-path",
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := longhornOwnedRuntimeObject(test.live); got != test.owned {
				t.Fatalf("longhornOwnedRuntimeObject() = %t, want %t", got, test.owned)
			}
		})
	}
}

func longhornRuntimeTestObject(t *testing.T, kind, name, webhookNamespace string) *unstructured.Unstructured {
	t.Helper()
	object := clusterScopedIdentity("admissionregistration.k8s.io/v1", kind, name)
	if err := unstructured.SetNestedSlice(object.Object, []any{longhornWebhookEntry(webhookNamespace)}, "webhooks"); err != nil {
		t.Fatal(err)
	}
	return &object
}

func storageLifecycleObjects(t *testing.T, engine cluster.CSI) []unstructured.Unstructured {
	t.Helper()
	objects, err := storageObjectsForEngine(engine)
	if err != nil {
		t.Fatal(err)
	}
	return objects
}

func storageLifecycleTestMapper(objects []unstructured.Unstructured) meta.RESTMapper {
	versions := make(map[schema.GroupVersion]struct{})
	for _, object := range objects {
		versions[object.GroupVersionKind().GroupVersion()] = struct{}{}
	}
	groups := make([]schema.GroupVersion, 0, len(versions))
	for version := range versions {
		groups = append(groups, version)
	}
	mapper := meta.NewDefaultRESTMapper(groups)
	for _, object := range objects {
		scope := meta.RESTScopeRoot
		if object.GetNamespace() != "" {
			scope = meta.RESTScopeNamespace
		}
		mapper.Add(object.GroupVersionKind(), scope)
	}
	return mapper
}

func storageLifecycleTestResource(t *testing.T, client *dynamicfake.FakeDynamicClient, mapper meta.RESTMapper, object *unstructured.Unstructured) dynamic.ResourceInterface {
	t.Helper()
	mapping, err := mapper.RESTMapping(object.GroupVersionKind().GroupKind(), object.GroupVersionKind().Version)
	if err != nil {
		t.Fatal(err)
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		return client.Resource(mapping.Resource).Namespace(object.GetNamespace())
	}
	return client.Resource(mapping.Resource)
}

func createStorageLifecycleObjects(t *testing.T, client *dynamicfake.FakeDynamicClient, mapper meta.RESTMapper, objects []unstructured.Unstructured) {
	t.Helper()
	for i := range objects {
		object := storageLifecycleLiveForm(&objects[i])
		if _, err := storageLifecycleTestResource(t, client, mapper, object).Create(context.Background(), object, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// storageLifecycleLiveForm fills in what longhorn-manager writes on the
// cluster-scoped objects tbx renders as bare identities (longhornRuntimeObjects).
// The ownership guard reads exactly those fields, so a live cluster holds them
// and a fixture that omitted them would prove nothing.
func storageLifecycleLiveForm(object *unstructured.Unstructured) *unstructured.Unstructured {
	live := object.DeepCopy()
	switch live.GetKind() {
	case "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration":
		if err := unstructured.SetNestedSlice(live.Object, []any{longhornWebhookEntry(longhornNamespace)}, "webhooks"); err != nil {
			panic(err)
		}
	case "StorageClass":
		if live.GetName() == longhornStaticStorageClass {
			if err := unstructured.SetNestedField(live.Object, longhornProvisioner, "provisioner"); err != nil {
				panic(err)
			}
		}
	}
	return live
}

func longhornWebhookEntry(namespace string) map[string]any {
	return map[string]any{
		"name": "validator.longhorn.io",
		"clientConfig": map[string]any{
			"service": map[string]any{
				"name":      longhornAdmissionWebhookService,
				"namespace": namespace,
			},
		},
	}
}

func firstStorageLifecycleObject(t *testing.T, objects []unstructured.Unstructured, keep func(unstructured.Unstructured) bool) *unstructured.Unstructured {
	t.Helper()
	for i := range objects {
		if keep(objects[i]) {
			return &objects[i]
		}
	}
	t.Fatal("no matching storage lifecycle object")
	return nil
}

func storageLifecycleDeleteActions(actions []k8stesting.Action) []k8stesting.DeleteAction {
	deletes := make([]k8stesting.DeleteAction, 0, len(actions))
	for _, action := range actions {
		deleteAction, ok := action.(k8stesting.DeleteAction)
		if ok {
			deletes = append(deletes, deleteAction)
		}
	}
	return deletes
}

func storageLifecycleResourceName(t *testing.T, mapper meta.RESTMapper, object *unstructured.Unstructured) string {
	t.Helper()
	mapping, err := mapper.RESTMapping(object.GroupVersionKind().GroupKind(), object.GroupVersionKind().Version)
	if err != nil {
		t.Fatal(err)
	}
	return mapping.Resource.Resource
}
