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
	subnetWarning, err := cluster.CheckSubnetIndex(item.SubnetIndex, s.hostSubnetSources())
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
			warning := "saved state could not be restored; cold-booting instead"
			if saveErr != nil {
				warning = "no saved state found; cold-booting instead"
			}
			log.Printf("resume %s: %s: %v", node.Name, warning, fallbackErr)
			nodeWarning = fmt.Sprintf("%s: %s", node.Name, warning)
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
	result.Warning = joinWarnings(append([]string{subnetWarning}, warnings...)...)
	return result, nil
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

func saveStatePath(dir, node string) string {
	return filepath.Join(dir, node+".vzstate")
}
