package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/vm"
)

type savedMachine interface {
	Suspend(string) error
	StopAfterSave() error
	Close() error
}

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
		nodes = make(map[string]*vm.VM)
		s.vms[item.Name] = nodes
	}
	warnings := []string{subnetWarning}
	var attempted []string
	for _, node := range item.Nodes {
		machine, err := machineForResume(nodes[node.Name], func() (*vm.VM, error) {
			return newVM(item, node)
		})
		if err != nil {
			rollbackErr := s.closeNodes(item.Name, nodes, attempted)
			return ClusterSummary{}, errors.Join(fmt.Errorf("create VM %s: %w", node.Name, err), rollbackErr)
		}
		nodes[node.Name] = machine
		attempted = append(attempted, node.Name)
		savePath := saveStatePath(dir, node.Name)
		_, saveErr := os.Stat(savePath)
		warning, resumeErr := resumeNode(saveErr == nil,
			func() error { return machine.RestoreState(savePath) },
			machine.Start,
		)
		_ = os.Remove(savePath) // consumed, unusable, or absent — never reuse a stale save
		if resumeErr != nil {
			rollbackErr := s.closeNodes(item.Name, nodes, attempted)
			return ClusterSummary{}, errors.Join(fmt.Errorf("resume %s: %w", node.Name, resumeErr), rollbackErr)
		}
		if warning != "" {
			log.Printf("resume %s: %s", node.Name, warning)
			warnings = append(warnings, fmt.Sprintf("%s: %s", node.Name, warning))
		}
	}
	go s.bindMirrors(item.SubnetIndex) // resume bypasses start(); rebind the gateway
	result := summary(item, true)
	result.Warning = joinWarnings(warnings...)
	return result, nil
}

// prepareSavedMachine leaves a successfully-saved VM stopped but otherwise
// intact. On failure, retain reports whether cleanup failed and the daemon
// must keep tracking the machine so a later lifecycle operation can retry it.
func prepareSavedMachine(machine savedMachine, savePath string) (retain bool, err error) {
	if err := machine.Suspend(savePath); err != nil {
		closeErr := machine.Close()
		return closeErr != nil, errors.Join(err, closeErr)
	}
	if err := machine.StopAfterSave(); err != nil {
		closeErr := machine.Close()
		return closeErr != nil, errors.Join(err, closeErr)
	}
	return true, nil
}

func machineForResume(retained *vm.VM, create func() (*vm.VM, error)) (*vm.VM, error) {
	if retained != nil {
		return retained, nil
	}
	return create()
}

// resumeNode tries to restore a node from its saved state; on a missing or
// unusable save it falls back to a cold boot and returns a warning. Only a
// cold-boot failure (nothing left to try) is fatal.
func resumeNode(saveExists bool, restore, coldStart func() error) (warning string, err error) {
	if saveExists {
		if err := restore(); err == nil {
			return "", nil
		}
		warning = "saved state could not be restored; cold-booting instead"
	} else {
		warning = "no saved state found; cold-booting instead"
	}
	if err := coldStart(); err != nil {
		return "", err
	}
	return warning, nil
}

func saveStatePath(dir, node string) string {
	return filepath.Join(dir, node+".vzstate")
}
