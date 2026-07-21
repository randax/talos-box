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

// suspendCluster pauses and saves every running VM's state, then closes them.
// The cluster is left "suspended": VMs stopped, save files on disk.
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
		if err := machine.Suspend(savePath); err != nil {
			errs = append(errs, fmt.Errorf("suspend %s: %w", name, err))
			_ = os.Remove(savePath) // no partial save left behind
		}
		// always release the VM: a failed Suspend leaves it paused, and the
		// map is dropped below — closing here prevents a leaked vz VM still
		// holding its network fd and MAC
		if err := machine.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", name, err))
		}
	}
	delete(s.vms, item.Name)
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
	nodes := s.vms[item.Name]
	if nodes == nil {
		nodes = make(map[string]*vm.VM)
		s.vms[item.Name] = nodes
	}
	for _, node := range item.Nodes {
		machine, err := newVM(item, node)
		if err != nil {
			return ClusterSummary{}, fmt.Errorf("create VM %s: %w", node.Name, err)
		}
		nodes[node.Name] = machine
		savePath := saveStatePath(dir, node.Name)
		_, saveErr := os.Stat(savePath)
		warning, resumeErr := resumeNode(saveErr == nil,
			func() error { return machine.RestoreState(savePath) },
			machine.Start,
		)
		_ = os.Remove(savePath) // consumed, unusable, or absent — never reuse a stale save
		if resumeErr != nil {
			return ClusterSummary{}, fmt.Errorf("resume %s: %w", node.Name, resumeErr)
		}
		if warning != "" {
			log.Printf("resume %s: %s", node.Name, warning)
		}
	}
	return summary(item, true), nil
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
