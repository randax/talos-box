package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

// writeClusterKubeconfig gives a cluster the admin credentials the Kubernetes
// node deletion requires before it will talk to an API server at all.
func writeClusterKubeconfig(t *testing.T, name string) {
	t.Helper()
	dir, err := cluster.Dir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kubeconfig"), []byte("kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNodeRemoveAnswersWithoutWaitingForTheReconcile(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	releaseReconcile := make(chan struct{})
	t.Cleanup(func() { close(releaseReconcile) })
	service.provisionReconcile = func(ctx context.Context, _ provision.Request) (provision.Result, error) {
		select {
		case <-releaseReconcile:
		case <-ctx.Done():
		}
		return provision.Result{}, nil
	}

	done := make(chan Response, 1)
	go func() { done <- dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false) }()

	select {
	case response := <-done:
		if !response.OK {
			t.Fatalf("node.remove failed: %s", response.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("node.remove blocked on the follow-up reconcile instead of answering once the node was gone")
	}
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Nodes) != 2 {
		t.Fatalf("cluster node count after remove = %d, want 2", len(reloaded.Nodes))
	}
}

func TestNodeRemoveDeletesTheKubernetesNodeObject(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	writeClusterKubeconfig(t, item.Name)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) { return 0, nil }
	deleted := make(chan string, 1)
	service.deleteKubernetesNode = func(_ context.Context, observed cluster.Cluster, node string) error {
		if observed.Name != item.Name {
			t.Errorf("node deletion ran against cluster %q, want %q", observed.Name, item.Name)
		}
		deleted <- node
		return nil
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false)

	if !response.OK {
		t.Fatalf("node.remove failed: %s", response.Error)
	}
	select {
	case node := <-deleted:
		if node != "demo-worker-2" {
			t.Fatalf("deleted Kubernetes node %q, want demo-worker-2", node)
		}
	default:
		t.Fatal("node.remove left the Kubernetes node object behind")
	}
	if status := decodeNodeStatus(t, response); status.Warning != "" {
		t.Fatalf("successful remove warned %q, want no warning", status.Warning)
	}
}

func TestNodeRemoveDegradesFailedKubernetesNodeDeletionToAWarning(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	writeClusterKubeconfig(t, item.Name)
	service.deleteKubernetesNode = func(context.Context, cluster.Cluster, string) error {
		return errors.New("api server unreachable")
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false)

	if !response.OK {
		t.Fatalf("node.remove failed on a best-effort node deletion: %s", response.Error)
	}
	status := decodeNodeStatus(t, response)
	for _, want := range []string{"demo-worker-2", "api server unreachable", "kubectl delete node"} {
		if !strings.Contains(status.Warning, want) {
			t.Fatalf("node-deletion warning %q does not mention %q", status.Warning, want)
		}
	}
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Nodes) != 2 {
		t.Fatalf("cluster node count after remove = %d, want 2", len(reloaded.Nodes))
	}
}

func TestNodeRemoveSkipsKubernetesNodeDeletionOnAStoppedCluster(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	writeClusterKubeconfig(t, item.Name)
	delete(service.vms, item.Name)
	service.deleteKubernetesNode = func(context.Context, cluster.Cluster, string) error {
		t.Error("Kubernetes node deletion ran against a stopped cluster")
		return nil
	}

	if response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false); !response.OK {
		t.Fatalf("node.remove on a stopped cluster failed: %s", response.Error)
	}
}
