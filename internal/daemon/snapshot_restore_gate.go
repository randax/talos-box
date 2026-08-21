package daemon

import (
	"fmt"
	"slices"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
)

// nodeVolumeData is one vanishing node's verified volume count.
type nodeVolumeData struct {
	node  string
	count int
}

// gateSnapshotRestore observes, best-effort, whether the nodes a restore would
// delete — live nodes the snapshot never captured — hold storage volume data,
// before snapshot.restore takes the operation lock (the same
// unlocked-observation shape as gateNodeRemoval). Verified volumes refuse the
// restore unless force is set; a stopped or unreachable cluster never blocks
// the restore — per the #150 lifecycle principle — and degrades to a data-loss
// warning instead. It returns the warning to attach to the restore's status,
// or the refusal error.
func (s *Server) gateSnapshotRestore(args snapshotArgs) (string, error) {
	item, err := cluster.Load(args.Cluster)
	if err != nil {
		// the locked handler owns reporting load failures
		return "", nil
	}
	if item.CSI == "" {
		return "", nil
	}
	captured, err := cluster.SnapshotNodes(item.Name, args.Name)
	if err != nil {
		// the locked handler owns the invalid, missing, or unreadable snapshot
		// error, and cluster.RestoreSnapshot validates the captured state
		// before it deletes anything, so skipping the gate here cannot let a
		// broken snapshot destroy disks unobserved
		return "", nil
	}
	vanishing := vanishingRestoreNodes(item.Nodes, captured)
	if len(vanishing) == 0 {
		return "", nil
	}
	s.opMu.Lock()
	running := s.clusterRunning(item.Name)
	s.opMu.Unlock()
	if !running {
		return restoreUnverifiedDataWarning(item.CSI, args.Name, nodeNames(vanishing)), nil
	}
	countVolumes := s.nodeVolumeCount
	if countVolumes == nil {
		countVolumes = countNodeRemovalStorageVolumes
	}
	var counted []nodeVolumeData
	var unverified []string
	total := 0
	for _, node := range vanishing {
		// Count against no remaining nodes: a restore reverts the surviving
		// nodes' disks to the snapshot too, so a replica or PV elsewhere is not
		// a copy that survives this operation.
		observed := item
		observed.Nodes = []cluster.Node{node}
		count, err := s.countNodeVolumes(countVolumes, observed, node.Name)
		if err != nil {
			// one unverifiable node must not discard another's verified count
			unverified = append(unverified, node.Name)
			continue
		}
		if count > 0 {
			counted = append(counted, nodeVolumeData{node: node.Name, count: count})
			total += count
		}
	}
	switch {
	case total > 0 && !args.Force:
		return "", restoreVolumesBlockRestore(item, args.Name, counted, unverified)
	case total > 0:
		return restoreDataLossWarning(item.CSI, args.Name, counted, unverified), nil
	case len(unverified) > 0:
		return restoreUnverifiedDataWarning(item.CSI, args.Name, unverified), nil
	}
	return "", nil
}

// countNodeVolumes bounds each node's observation separately, so a slow first
// node cannot spend the whole budget and report the rest as unverifiable.
func (s *Server) countNodeVolumes(count nodeVolumeCountFunc, observed cluster.Cluster, nodeName string) (int, error) {
	ctx, cancel := s.lifecycleTimeoutContext(nodeVolumeCountTimeout)
	defer cancel()
	return count(ctx, observed, nodeName)
}

// vanishingRestoreNodes returns the live nodes the snapshot did not capture,
// whose disks the restore deletes.
func vanishingRestoreNodes(live, captured []cluster.Node) []cluster.Node {
	var vanishing []cluster.Node
	for _, node := range live {
		if slices.ContainsFunc(captured, func(candidate cluster.Node) bool { return candidate.Name == node.Name }) {
			continue
		}
		vanishing = append(vanishing, node)
	}
	return vanishing
}

func restoreVolumesBlockRestore(item cluster.Cluster, snapshot string, counted []nodeVolumeData, unverified []string) error {
	return fmt.Errorf(
		"cluster %q: restoring snapshot %q deletes %s, holding %s volume data%s; delete the volumes first, or rerun with --force to permanently delete their data",
		item.Name,
		snapshot,
		nodeVolumeList(counted),
		item.CSI,
		unverifiedNodesSuffix(unverified),
	)
}

func restoreDataLossWarning(csi cluster.CSI, snapshot string, counted []nodeVolumeData, unverified []string) string {
	return fmt.Sprintf(
		"restoring snapshot %s permanently deletes %s volume data on %s%s",
		snapshot,
		csi,
		nodeVolumeList(counted),
		unverifiedNodesSuffix(unverified),
	)
}

// restoreUnverifiedDataWarning must read honestly both after a completed
// restore and appended to a failure that may have stopped short of deleting
// the disks, so it ties the data loss to the disk deletion rather than
// asserting it happened.
func restoreUnverifiedDataWarning(csi cluster.CSI, snapshot string, nodes []string) string {
	return fmt.Sprintf(
		"restoring snapshot %s deletes %s %s but could not verify %s volume data; any volume data on them is permanently deleted with their disks — verify your volumes once the cluster is reachable",
		snapshot,
		nodeUnit(nodes),
		strings.Join(nodes, ", "),
		csi,
	)
}

func unverifiedNodesSuffix(unverified []string) string {
	if len(unverified) == 0 {
		return ""
	}
	return fmt.Sprintf(" (volume data on %s could not be verified)", strings.Join(unverified, ", "))
}

// nodeVolumeList reports counts per node: a volume replicated across two
// vanishing nodes must not read as two lost volumes.
func nodeVolumeList(counted []nodeVolumeData) string {
	entries := make([]string, 0, len(counted))
	for _, item := range counted {
		entries = append(entries, fmt.Sprintf("%s (%d %s)", item.node, item.count, Unit(item.count, "volume", "volumes")))
	}
	return strings.Join(entries, ", ")
}

func nodeNames(nodes []cluster.Node) []string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return names
}

func nodeUnit(nodes []string) string {
	if len(nodes) == 1 {
		return "node"
	}
	return "nodes"
}
