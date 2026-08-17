package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// suspendCluster pauses and saves every running VM's state, then stops them
// while retaining their device configurations for same-daemon restoration.
func (s *Server) suspendCluster(raw json.RawMessage) (ClusterSummary, error) {
	var args nameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	if !s.clusterRunning(item.Name) {
		return ClusterSummary{}, fmt.Errorf("cluster %q is not running", item.Name)
	}
	capability := s.hypervisor.Capabilities().Suspend
	if !capability.Supported {
		return ClusterSummary{}, fmt.Errorf("%w: %s", hypervisor.ErrUnsupported, capability.Reason)
	}
	s.cancelProvisionLocked(item.Name)
	s.invalidateStoragePhaseLocked(item.Name)
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	nodes := s.vms[item.Name]
	var errs []error
	for name, machine := range nodes {
		savePath := saveStatePath(dir, name)
		retain, err := prepareSavedMachine(machine, savePath)
		if err != nil {
			errs = append(errs, fmt.Errorf("suspend %s: %w", name, err))
			_ = os.Remove(savePath) // no partial save left behind
			if !retain {
				delete(nodes, name)
			}
		}
	}
	if len(nodes) == 0 {
		delete(s.vms, item.Name)
	}
	if len(errs) > 0 {
		return ClusterSummary{}, errors.Join(errs...)
	}
	return summary(item, false), nil
}

// resumeCluster brings a suspended cluster back: each node restores from its
// saved state, or cold-boots with a warning if the save is missing/corrupt.
func (s *Server) resumeCluster(raw json.RawMessage) (ClusterSummary, error) {
	var args nameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	if s.clusterRunning(item.Name) {
		return ClusterSummary{}, fmt.Errorf("cluster %q is already running", item.Name)
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	// Suspend deliberately leaves the cluster's bridge up, so its own subnet
	// occupancy is expected here: inspect for advisory findings only, never
	// re-decide a subnet the cluster already owns (#271).
	subnetWarning, err := cluster.AttachedSubnetWarning(item.SubnetIndex, s.hostSubnetSources())
	if err != nil {
		return ClusterSummary{}, err
	}
	nodes := s.vms[item.Name]
	if nodes == nil {
		nodes = make(map[string]hypervisor.Machine)
		s.vms[item.Name] = nodes
	}
	var attempted []string
	warnings, err := resumeNodeBatch(item.Nodes, func(node cluster.Node) (resumedNode, error) {
		savePath := saveStatePath(dir, node.Name)
		_, saveErr := os.Stat(savePath)
		var fallbackErr error
		delete(nodes, node.Name)
		machine, err := s.launchMachine(item, node, &hypervisor.Restore{
			Path: savePath,
			Fallback: func(err error) {
				fallbackErr = err
			},
		})
		if err != nil {
			return resumedNode{}, fmt.Errorf("resume %s: %w", node.Name, err)
		}
		nodes[node.Name] = machine
		attempted = append(attempted, node.Name)
		var nodeWarning string
		if fallbackErr != nil {
			nodeWarning = coldBootWarning(node.Name, saveErr != nil, fallbackErr)
			log.Printf("resume %s: %v", node.Name, fallbackErr)
		}
		return resumedNode{savePath: savePath, warning: nodeWarning}, nil
	}, func() error {
		return s.closeNodes(item.Name, nodes, attempted)
	})
	if err != nil {
		return ClusterSummary{}, err
	}
	go s.bindMirrors(item.SubnetIndex) // resume bypasses start(); rebind the gateway
	result := summary(item, true)
	result.setWarnings(append([]string{subnetWarning}, warnings...)...)
	return result, nil
}

// coldBootWarning explains a cold boot the way the daemon log already does:
// with the hypervisor's own reason. A warning that only says the restore
// failed leaves the operator nothing to act on (#291).
func coldBootWarning(nodeName string, saveMissing bool, cause error) string {
	if saveMissing {
		return fmt.Sprintf("%s: no saved state found; cold-booting instead", nodeName)
	}
	warning := fmt.Sprintf("%s: saved state could not be restored; cold-booting instead", nodeName)
	if cause == nil {
		return warning + " (details: ~/.talosbox/tbxd.log)"
	}
	return fmt.Sprintf("%s: %v (details: ~/.talosbox/tbxd.log)", warning, cause)
}

type resumedNode struct {
	savePath string
	warning  string
}

// resumeNodeBatch is the cluster-wide commit boundary: a failed node rolls
// back attempted VMs without consuming any saved states, while full success
// consumes the complete batch together.
func resumeNodeBatch[T any](nodes []T, resume func(T) (resumedNode, error), rollback func() error) ([]string, error) {
	var savePaths, warnings []string
	for _, node := range nodes {
		result, err := resume(node)
		if err != nil {
			return nil, errors.Join(err, rollback())
		}
		savePaths = append(savePaths, result.savePath)
		if result.warning != "" {
			warnings = append(warnings, result.warning)
		}
	}
	removeSaveStateFiles(savePaths)
	return warnings, nil
}

// removeSaveStateFiles commits a successful cluster-wide resume. Callers keep
// the batch intact on rollback so a later resume can retry every saved node.
func removeSaveStateFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

// prepareSavedMachine leaves a successfully-saved VM stopped but otherwise
// intact. On failure, retain reports whether the daemon must keep tracking the
// machine. A failed stop is always retained because Close does not prove that
// the machine stopped.
func prepareSavedMachine(machine hypervisor.Machine, savePath string) (retain bool, err error) {
	if err := machine.Suspend(context.Background(), savePath); err != nil {
		closeErr := closeMachine(machine)
		return closeErr != nil, errors.Join(err, closeErr)
	}
	if err := stopMachine(machine); err != nil {
		closeErr := machine.Close()
		return true, errors.Join(err, closeErr)
	}
	return true, nil
}

// saveStateSuffix names every file suspend writes, so the presence of saved
// memory can be detected without knowing the node names.
const saveStateSuffix = ".vzstate"

func saveStatePath(dir, node string) string {
	return filepath.Join(dir, node+saveStateSuffix)
}

// discardSavedState drops a node's suspended memory before it cold-boots. A
// save is only consumed by a successful resume, so a start that boots the node
// from disk would otherwise leave a stale save behind: status keeps reporting
// the cluster Suspended and the resume hint invites a restore onto memory that
// no longer matches what is running. It reports whether a save was discarded,
// plus an operator-visible warning when a save was found but could not be
// removed — the cold boot proceeds either way, and the survivor would silently
// resurrect the suspended status and the resume hint.
func discardSavedState(dir, nodeName string) (bool, string) {
	path := saveStatePath(dir, nodeName)
	if _, err := os.Stat(path); err != nil {
		return false, ""
	}
	if err := os.Remove(path); err != nil {
		log.Printf("discard saved state %s: %v", path, err)
		return false, undiscardedSaveStateWarning(nodeName, err)
	}
	log.Printf("discarded saved state %s: cold boot", path)
	return true, ""
}

// discardedSaveStateWarning tells the operator that a cold boot threw suspended
// memory away, because the discard is otherwise invisible: the next status just
// stops saying Suspended.
func discardedSaveStateWarning(subject string) string {
	return fmt.Sprintf("discarded suspended memory state; %s cold-booted", subject)
}

// undiscardedSaveStateWarning is the other half: the cold boot happened but the
// save outlived it, so status will keep calling the node suspended and the hint
// will keep offering a resume onto memory the running VM no longer matches.
func undiscardedSaveStateWarning(nodeName string, err error) string {
	return fmt.Sprintf(
		"could not discard suspended memory state for %s: %v; ignore the suspended status and do not resume",
		nodeName, err,
	)
}

// nodeHasSavedState reports whether one node's own memory is saved on disk.
// suspend only writes a save for nodes that were running, so this is what
// separates the members a resume restores from those it cold-boots.
func nodeHasSavedState(clusterName, nodeName string) bool {
	dir, err := cluster.Dir(clusterName)
	if err != nil {
		return false
	}
	_, err = os.Stat(saveStatePath(dir, nodeName))
	return err == nil
}

// clusterHasSavedState reports whether a cluster still holds suspended VM
// memory on disk. Suspension is otherwise invisible after a daemon restart —
// the tracked VMs are gone — so this on-disk signal is what tells a client that
// restarting tbxd would discard the cluster's saved memory.
func clusterHasSavedState(name string) bool {
	dir, err := cluster.Dir(name)
	if err != nil {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*"+saveStateSuffix))
	return err == nil && len(matches) > 0
}
