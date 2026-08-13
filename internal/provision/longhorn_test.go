package provision

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"go.yaml.in/yaml/v4"
	"helm.sh/helm/v3/pkg/chart/loader"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestRenderLonghornUsesPinnedChartAndImages(t *testing.T) {
	item, err := cluster.New("demo", 0, 1, 2, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn}
	objects, err := renderLonghorn(item)
	if err != nil {
		t.Fatal(err)
	}
	var (
		namespace    *unstructured.Unstructured
		storageClass *unstructured.Unstructured
	)
	images := map[string]bool{}
	for i := range objects {
		object := &objects[i]
		if labels := object.GetLabels(); labels["talosbox.dev/managed"] != "true" {
			t.Fatalf("%s/%s labels = %v, want talosbox.dev/managed", object.GetKind(), object.GetName(), labels)
		}
		switch {
		case object.GetKind() == "Namespace" && object.GetName() == longhornNamespace:
			namespace = object
		case object.GetKind() == "StorageClass" && object.GetName() == longhornStorageClass:
			storageClass = object
		}
		collectImageReferences(object.Object, images)
	}
	if namespace == nil || storageClass == nil {
		t.Fatalf("renderLonghorn() missing namespace=%v storageClass=%v", namespace != nil, storageClass != nil)
	}
	if got, _, err := nestedStringField(namespace.Object, "metadata", "labels", "pod-security.kubernetes.io/enforce"); err != nil || got != "privileged" {
		t.Fatalf("namespace PSA enforce = %q, %v", got, err)
	}
	if got, _, err := nestedStringField(storageClass.Object, "metadata", "annotations", "storageclass.kubernetes.io/is-default-class"); err != nil || got != "true" {
		t.Fatalf("default StorageClass annotation = %q, %v", got, err)
	}
	parameters, found, err := unstructured.NestedStringMap(storageClass.Object, "parameters")
	if err != nil || !found {
		t.Fatalf("storage class parameters found=%v err=%v", found, err)
	}
	if parameters["numberOfReplicas"] != "3" {
		t.Fatalf("StorageClass numberOfReplicas = %q, want %q", parameters["numberOfReplicas"], "3")
	}
	expectedImages := []string{
		"docker.io/longhornio/backing-image-manager:v1.12.0",
		"docker.io/longhornio/csi-attacher:v4.12.0",
		"docker.io/longhornio/csi-node-driver-registrar:v2.17.0",
		"docker.io/longhornio/csi-provisioner:v5.3.0-20260514",
		"docker.io/longhornio/csi-resizer:v2.1.0-20260514",
		"docker.io/longhornio/csi-snapshotter:v8.5.0-20260514",
		"docker.io/longhornio/livenessprobe:v2.19.0",
		"docker.io/longhornio/longhorn-engine:v1.12.0",
		"docker.io/longhornio/longhorn-instance-manager:v1.12.0",
		"docker.io/longhornio/longhorn-manager:v1.12.0",
		"docker.io/longhornio/longhorn-share-manager:v1.12.0",
		"docker.io/longhornio/longhorn-ui:v1.12.0",
		"docker.io/longhornio/support-bundle-kit:v0.0.86",
	}
	for _, image := range expectedImages {
		if !images[image] {
			t.Fatalf("rendered images missing %q; got %v", image, sortedKeys(images))
		}
	}
}

func TestEmbeddedLonghornChartPinsEveryCuratedImage(t *testing.T) {
	chart, err := loader.LoadArchive(bytes.NewReader(longhornChart))
	if err != nil {
		t.Fatal(err)
	}
	encodedBytes, err := yaml.Marshal(chart.Values)
	if err != nil {
		t.Fatal(err)
	}
	encodedValues := string(encodedBytes)
	for _, expected := range []string{
		"longhornio/backing-image-manager",
		"v1.12.0",
		"longhornio/csi-attacher",
		"v4.12.0",
		"longhornio/csi-node-driver-registrar",
		"v2.17.0",
		"longhornio/csi-provisioner",
		"v5.3.0-20260514",
		"longhornio/csi-resizer",
		"v2.1.0-20260514",
		"longhornio/csi-snapshotter",
		"v8.5.0-20260514",
		"longhornio/livenessprobe",
		"v2.19.0",
		"longhornio/longhorn-engine",
		"longhornio/longhorn-instance-manager",
		"longhornio/longhorn-manager",
		"longhornio/longhorn-share-manager",
		"longhornio/longhorn-ui",
		"longhornio/support-bundle-kit",
		"v0.0.86",
	} {
		if !strings.Contains(encodedValues, expected) {
			t.Fatalf("embedded chart values missing %q", expected)
		}
	}
}

func TestRenderLonghornReplicasFollowNodeCount(t *testing.T) {
	tests := []struct {
		nodes int
		want  string
	}{
		{nodes: 1, want: "1"},
		{nodes: 2, want: "2"},
		{nodes: 3, want: "3"},
		{nodes: 4, want: "3"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d-nodes", tt.nodes), func(t *testing.T) {
			item, err := cluster.New("demo", 0, 1, tt.nodes-1, cluster.NodeDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn}
			objects, err := renderLonghorn(item)
			if err != nil {
				t.Fatal(err)
			}
			storageClass := findObject(t, objects, "StorageClass", longhornStorageClass)
			parameters, found, err := unstructured.NestedStringMap(storageClass.Object, "parameters")
			if err != nil || !found {
				t.Fatalf("storage class parameters found=%v err=%v", found, err)
			}
			if parameters["numberOfReplicas"] != tt.want {
				t.Fatalf("StorageClass numberOfReplicas = %q, want %q", parameters["numberOfReplicas"], tt.want)
			}
		})
	}
}

func TestRenderLonghornRejectsNonLonghornIntent(t *testing.T) {
	_, err := renderLonghorn(cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath}})
	if err == nil || !strings.Contains(err.Error(), "csi: longhorn") {
		t.Fatalf("renderLonghorn() error = %v", err)
	}
}

func TestWaitForLonghornRequiresReadyComponents(t *testing.T) {
	ready := kubernetesfake.NewClientset(
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: longhornManagerName, Namespace: longhornNamespace, Generation: 1},
			Status:     appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, NumberReady: 1},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: longhornDriverDeployerName, Namespace: longhornNamespace, Generation: 1},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: longhornUIName, Namespace: longhornNamespace, Generation: 1},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1},
		},
	)
	if err := waitForLonghorn(context.Background(), ready, time.Millisecond); err != nil {
		t.Fatal(err)
	}

	notReady := kubernetesfake.NewClientset(
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: longhornManagerName, Namespace: longhornNamespace, Generation: 1},
			Status:     appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, NumberReady: 0},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: longhornDriverDeployerName, Namespace: longhornNamespace, Generation: 1},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: longhornUIName, Namespace: longhornNamespace, Generation: 1},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1},
		},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := waitForLonghorn(ctx, notReady, time.Millisecond); err == nil {
		t.Fatal("waitForLonghorn() accepted unready components")
	}
}

func findObject(t *testing.T, objects []unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for i := range objects {
		object := &objects[i]
		if object.GetKind() == kind && object.GetName() == name {
			return object
		}
	}
	t.Fatalf("missing %s %q", kind, name)
	return nil
}

func collectImageReferences(value any, images map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for _, entry := range typed {
			collectImageReferences(entry, images)
		}
	case []any:
		for _, entry := range typed {
			collectImageReferences(entry, images)
		}
	case string:
		if strings.Contains(typed, "/") && strings.Contains(typed, ":") && strings.Contains(typed, "longhornio/") {
			images[typed] = true
		}
	}
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
