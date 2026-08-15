package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

const nodeVolumeCountTimeout = 10 * time.Second

// gateNodeRemoval observes, best-effort, whether the node holds the only copy
// of curated storage volumes before node.remove takes the operation lock (the
// same unlocked-observation shape as dispatchProvisioning). Verified volumes
// on the node refuse the removal unless force is set; a stopped or
// unreachable cluster never blocks removal — per the #150 lifecycle principle
// — and degrades to a data-loss warning instead. It returns the warning to
// attach to the removal's NodeStatus, or the refusal error.
func (s *Server) gateNodeRemoval(raw json.RawMessage) (string, error) {
	var args nodeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return "", err
	}
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		// the locked handler owns reporting load failures
		return "", nil
	}
	if item.CSI == "" {
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
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return 0, err
	}
	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		return 0, fmt.Errorf("read kubeconfig for node volume count: %w", err)
	}
	return provision.CountNodeStorageVolumes(ctx, kubeconfig, item.CSI, nodeName)
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

func removeNodeUnverifiedDataWarning(item cluster.Cluster, nodeName string) string {
	return fmt.Sprintf(
		"removing node %s may permanently delete %s volume data; inspect persistent volumes manually if you need to keep it",
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
