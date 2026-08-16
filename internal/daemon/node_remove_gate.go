package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

const nodeVolumeCountTimeout = 10 * time.Second

// nodeVolumeCountFunc counts the storage volumes whose data lives only on one
// node, weighed against the remaining nodes in the passed cluster model.
type nodeVolumeCountFunc func(context.Context, cluster.Cluster, string) (int, error)

// gateNodeRemoval observes, best-effort, whether the node holds the only copy
// of curated storage volumes before node.remove takes the operation lock (the
// same unlocked-observation shape as dispatchProvisioning). Verified volumes
// on the node refuse the removal unless force is set; a stopped or
// unreachable cluster never blocks removal — per the #150 lifecycle principle
// — and degrades to a data-loss warning instead. It returns the warning to
// attach to the removal's NodeStatus, or the refusal error.
func (s *Server) gateNodeRemoval(args nodeArgs) (string, error) {
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		// the locked handler owns reporting load failures
		return "", nil
	}
	if item.CSI == "" {
		return "", nil
	}
	member := slices.ContainsFunc(item.Nodes, func(node cluster.Node) bool {
		return node.Name == args.Name
	})
	if !member {
		// the locked handler owns the no-such-node error; gating a stale PV's
		// node name would misreport a typo as held volume data
		return "", nil
	}
	s.opMu.Lock()
	running := s.clusterRunning(item.Name)
	s.opMu.Unlock()
	if !running {
		return removeNodeUnverifiedDataWarning(item, args.Name), nil
	}
	countVolumes := s.nodeVolumeCount
	if countVolumes == nil {
		countVolumes = countNodeRemovalStorageVolumes
	}
	ctx, cancel := s.lifecycleTimeoutContext(nodeVolumeCountTimeout)
	defer cancel()
	count, err := countVolumes(ctx, item, args.Name)
	if err != nil {
		return removeNodeUnverifiedDataWarning(item, args.Name), nil
	}
	if count == 0 {
		return "", nil
	}
	if !args.Force {
		return "", removeNodeVolumesBlockRemoval(item, args.Name, count)
	}
	return removeNodeDataLossWarning(item, args.Name, count), nil
}

func countNodeRemovalStorageVolumes(ctx context.Context, item cluster.Cluster, nodeName string) (int, error) {
	kubeconfig, err := clusterKubeconfig(item.Name)
	if err != nil {
		return 0, fmt.Errorf("read kubeconfig for node volume count: %w", err)
	}
	remaining := make([]string, 0, len(item.Nodes))
	for _, node := range item.Nodes {
		if node.Name != nodeName {
			remaining = append(remaining, node.Name)
		}
	}
	return provision.CountNodeStorageVolumes(ctx, kubeconfig, item.CSI, nodeName, remaining)
}

func removeNodeVolumesBlockRemoval(item cluster.Cluster, nodeName string, count int) error {
	return fmt.Errorf(
		"cluster %q: node %q holds the only copy of %d %s %s; delete the volumes first, or rerun with --force to permanently delete their data",
		item.Name,
		nodeName,
		count,
		item.CSI,
		volumeUnit(count),
	)
}

func removeNodeDataLossWarning(item cluster.Cluster, nodeName string, count int) string {
	return fmt.Sprintf(
		"removing node %s permanently deletes the only copy of %d %s %s",
		nodeName,
		count,
		item.CSI,
		volumeUnit(count),
	)
}

// removeNodeUnverifiedDataWarning must read honestly both after a completed
// removal and appended to a failure that may have stopped short of deleting
// the disk, so it ties the data loss to the disk deletion rather than
// asserting it happened.
func removeNodeUnverifiedDataWarning(item cluster.Cluster, nodeName string) string {
	return fmt.Sprintf(
		"removing node %s could not verify %s volume data; any volume whose only copy lives on it is permanently deleted with its disk — verify your volumes once the cluster is reachable",
		nodeName,
		item.CSI,
	)
}

func volumeUnit(count int) string {
	if count == 1 {
		return "volume"
	}
	return "volumes"
}
