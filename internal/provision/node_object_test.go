package provision

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestDeleteKubernetesNodeRemovesTheNodeObject(t *testing.T) {
	client := kubernetesfake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "demo-worker-2"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "demo-cp-1"}},
	)

	if err := deleteKubernetesNode(context.Background(), client, "demo-worker-2"); err != nil {
		t.Fatalf("delete node object: %v", err)
	}

	nodes, err := client.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes.Items) != 1 || nodes.Items[0].Name != "demo-cp-1" {
		t.Fatalf("surviving nodes = %+v, want only demo-cp-1", nodes.Items)
	}
}

func TestDeleteKubernetesNodeTreatsMissingNodeAsDone(t *testing.T) {
	client := kubernetesfake.NewClientset()

	if err := deleteKubernetesNode(context.Background(), client, "demo-worker-2"); err != nil {
		t.Fatalf("delete absent node object: %v, want success", err)
	}
}

func TestDeleteKubernetesNodeReportsOtherFailures(t *testing.T) {
	client := kubernetesfake.NewClientset()
	client.PrependReactor("delete", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "demo-worker-2", errors.New("nope"))
	})

	err := deleteKubernetesNode(context.Background(), client, "demo-worker-2")
	if err == nil {
		t.Fatal("delete node object succeeded, want the forbidden failure reported")
	}
}
