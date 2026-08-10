package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
)

type balloonMachine struct {
	machine                 hypervisor.Machine
	configuredMiB           int
	tolerateDeviceNotActive bool
}

func (m balloonMachine) ConfiguredMiB() int { return m.configuredMiB }

func (m balloonMachine) SetMemoryTargetMiB(targetMiB int) error {
	err := m.machine.SetMemoryTargetMiB(targetMiB)
	if m.tolerateDeviceNotActive && errors.Is(err, hypervisor.ErrDeviceNotActive) {
		return nil
	}
	return err
}

// Balloonables snapshots the CONFIGURED running nodes for the balloon manager,
// reading s.vms under opMu so the manager never races an op. Backends with
// guest-visible balloon readback can manage every active node directly. On VZ
// without readback, maintenance-mode nodes remain exempt (SPEC §8): their
// guest has no virtio_balloon driver, and setting a target on one crashes vz,
// so only TLS-configured nodes observed through apid are managed.
func (s *Server) Balloonables() map[string]balloon.Balloonable {
	type entry struct {
		machine                 hypervisor.Machine
		configuredMiB           int
		ip                      string
		tolerateDeviceNotActive bool
	}
	balloonReadback := s.hypervisor.Capabilities().BalloonReadback.Supported
	s.opMu.Lock()
	candidates := map[string]entry{}
	for clusterName, nodes := range s.vms {
		item, err := cluster.Load(clusterName)
		if err != nil {
			continue
		}
		byName := map[string]cluster.Node{}
		for _, n := range item.Nodes {
			byName[n.Name] = n
		}
		for nodeName, machine := range nodes {
			node, ok := byName[nodeName]
			if !ok || !machine.Active() {
				continue
			}
			candidates[clusterName+"/"+nodeName] = entry{
				machine:                 machine,
				configuredMiB:           item.DefaultsFor(node.Role).MemoryMiB,
				ip:                      cluster.LookupIP(node.MAC, item.SubnetIndex),
				tolerateDeviceNotActive: balloonReadback,
			}
		}
	}
	s.opMu.Unlock()

	out := map[string]balloon.Balloonable{}
	for key, e := range candidates {
		if balloonReadback || (e.ip != "" && ClassifyPhase(true, probeAPID(e.ip)) == PhaseConfigured) {
			out[key] = balloonMachine{
				machine:                 e.machine,
				configuredMiB:           e.configuredMiB,
				tolerateDeviceNotActive: e.tolerateDeviceNotActive,
			}
		}
	}
	return out
}

// checkOvercommit sums configured memory across all clusters plus the incoming
// addition; if it exceeds host RAM minus the reserve, it returns a warning
// unless force is set.
// checkOvercommit sums memory across all RUNNING clusters plus addMiB (the
// memory the pending operation adds) and warns if it exceeds host RAM minus the
// reserve. Returns a warning string for the caller to surface when forced.
func (s *Server) checkOvercommit(addMiB int, force bool) (string, error) {
	total, err := balloon.HostTotalMiB()
	if err != nil {
		return "", nil // can't read host RAM; don't block
	}
	reserve := balloon.DefaultConfig().ReserveMiB
	clusters, err := cluster.List()
	if err != nil {
		return "", err
	}
	planned := addMiB
	for _, item := range clusters {
		if s.clusterRunning(item.Name) {
			planned += clusterMemoryMiB(item)
		}
	}
	if !balloon.Overcommit(planned, total, reserve) {
		return "", nil
	}
	msg := fmt.Sprintf("planned VM memory %d MiB exceeds host %d MiB minus %d MiB reserve", planned, total, reserve)
	if !force {
		return "", fmt.Errorf("%s (use --force to override; ballooning will reclaim under pressure)", msg)
	}
	return msg + " (forced — ballooning will reclaim under pressure)", nil
}

func (s *Server) checkHostPressure(path string, force bool) (string, error) {
	measure := s.hostPressure
	if measure == nil {
		measure = hostpressure.SystemSnapshot
	}
	snapshot, err := measure(path)
	if err != nil {
		return fmt.Sprintf("host-pressure probe failed: %v; proceeding without host-pressure protection", err), nil
	}
	warning := strings.Join(hostpressure.Warnings(snapshot), "; ")
	if warning == "" {
		return "", nil
	}
	if !force {
		return "", fmt.Errorf("%s (use --force to override)", warning)
	}
	return warning + " (forced)", nil
}

func clusterMemoryMiB(item cluster.Cluster) int {
	total := 0
	for _, node := range item.Nodes {
		total += item.DefaultsFor(node.Role).MemoryMiB
	}
	return total
}
