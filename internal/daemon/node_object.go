package daemon

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

// kubernetesNodeDeleteTimeout bounds the Node-object deletion hard: it runs on
// the request path of `tbx node remove`, and a cluster that cannot answer must
// cost the operator seconds, not the provisioning budget (#314).
const kubernetesNodeDeleteTimeout = 15 * time.Second

// deleteRemovedKubernetesNode drops the Kubernetes Node object of a node that
// just left a provisioned cluster. Without it the node stays NotReady forever:
// its VM is gone, so nothing will ever mark it Ready again. Best-effort — the
// substrate removal already happened, so a failure degrades to a warning on the
// result and never fails the removal.
func (s *Server) deleteRemovedKubernetesNode(clusterName, nodeName string) string {
	item, err := cluster.Load(clusterName)
	if err != nil {
		// the cluster is gone or unreadable; there is nothing to reconcile against
		return ""
	}
	if item.CNI == "" {
		return ""
	}
	if _, err := clusterKubeconfig(item.Name); err != nil {
		// no admin credentials means the cluster was never brought up far enough
		// to have a Node object for this node
		log.Printf("node.remove %s/%s: no kubeconfig, skipping Kubernetes node deletion", item.Name, nodeName)
		return ""
	}
	s.opMu.Lock()
	running := s.clusterRunning(item.Name)
	s.opMu.Unlock()
	if !running {
		// A stopped cluster has no API server to talk to, and nothing deletes
		// the Node object later: the reconcile a start schedules is unforced and
		// readiness ignores unexpected nodes, so the removed member would sit
		// NotReady forever. Hand the operator the one command that clears it.
		log.Printf("node.remove %s/%s: cluster is stopped, Kubernetes node object left in place", item.Name, nodeName)
		return stoppedClusterKubernetesNodeWarning(nodeName)
	}
	log.Printf("node.remove %s/%s: deleting Kubernetes node object", item.Name, nodeName)
	deleteNode := s.deleteKubernetesNode
	if deleteNode == nil {
		deleteNode = deleteClusterKubernetesNode
	}
	ctx, cancel := s.lifecycleTimeoutContext(kubernetesNodeDeleteTimeout)
	defer cancel()
	if err := deleteNode(ctx, item, nodeName); err != nil {
		log.Printf("node.remove %s/%s: Kubernetes node object not deleted: %v", item.Name, nodeName, err)
		return kubernetesNodeDeleteWarning(nodeName, err)
	}
	log.Printf("node.remove %s/%s: Kubernetes node object deleted", item.Name, nodeName)
	return ""
}

func deleteClusterKubernetesNode(ctx context.Context, item cluster.Cluster, nodeName string) error {
	kubeconfig, err := clusterKubeconfig(item.Name)
	if err != nil {
		return fmt.Errorf("read kubeconfig for node deletion: %w", err)
	}
	return provision.DeleteKubernetesNode(ctx, kubeconfig, nodeName)
}

// stoppedClusterKubernetesNodeWarning is the stopped-cluster half: the member
// left state and disk, but its Kubernetes Node object is still registered and no
// later operation removes it, so the cleanup is the operator's to run once the
// cluster answers again.
func stoppedClusterKubernetesNodeWarning(nodeName string) string {
	return fmt.Sprintf(
		"node %s was removed while the cluster was stopped, so its Kubernetes node object still exists and stays NotReady; run `kubectl delete node %s` once the cluster is running again",
		nodeName,
		nodeName,
	)
}

func kubernetesNodeDeleteWarning(nodeName string, err error) string {
	return fmt.Sprintf(
		"node %s was removed but its Kubernetes node object could not be deleted (%v); it stays NotReady until you run `kubectl delete node %s`",
		nodeName,
		err,
		nodeName,
	)
}
