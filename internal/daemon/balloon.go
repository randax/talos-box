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
	measure := s.hostTotalMemory
	if measure == nil {
		measure = balloon.HostTotalMiB
	}
	total, err := measure()
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

func (s *Server) checkHostPressure(path string, force bool) ([]string, error) {
	snapshot, err := s.pressureSnapshot(path)
	if err != nil {
		return []string{fmt.Sprintf("host-pressure probe failed: %v; proceeding without host-pressure protection", err)}, nil
	}
	// hostpressure.Assess is the shared classification: tbx doctor reports the
	// same blocking findings as FAIL, so the gate and the diagnostic agree.
	// Findings stay one warning each so the CLI renders them one per line.
	return applyPressureFindings(hostpressure.Assess(snapshot), force)
}

// The host readings a Server falls back to when it was built without its own
// probes — every production Server sets all three in newServer, so the fallback
// only ever answers a Server assembled field by field. They are variables so
// the package's tests can pin them: a gate that reads the real machine turns
// every test that merely passes through it into a test of the runner's spare
// RAM.
var (
	measureHostFreeMiB  = balloon.HostFreeMiB
	measureHostTotalMiB = balloon.HostTotalMiB
	measureHostPressure = hostpressure.SystemSnapshot
)

// checkProvisionStart gates the *start* of new guests on the host as measured
// right now (#334). checkOvercommit only compares configured memory against
// total RAM, and checkHostPressure only judges the host as it stands; neither
// sees a second bringup arriving on a host whose free memory is already spoken
// for. That gap admitted the create that drove host swap to 7.3/8 GiB and
// panicked a worker guest mid-boot.
//
// Probe failures never block: an unmeasurable host falls back to the guards
// that came before this one, exactly as checkOvercommit does.
func (s *Server) checkProvisionStart(path string, addMiB int, force bool) ([]string, error) {
	freeMiB := 0
	measureFree := s.hostFreeMemory
	if measureFree == nil {
		measureFree = measureHostFreeMiB
	}
	if measured, err := measureFree(); err == nil {
		freeMiB = measured
	}
	// The free reading only means something against the host's size, which is
	// what tells a stale swap file on a roomy host from live pressure on a full
	// one. An unreadable total leaves the swap rule armed on the footprint alone.
	totalMiB := 0
	measureTotal := s.hostTotalMemory
	if measureTotal == nil {
		measureTotal = measureHostTotalMiB
	}
	if measured, err := measureTotal(); err == nil {
		totalMiB = measured
	}
	var swap hostpressure.Usage
	pressure := hostpressure.MemoryPressureUnknown
	if snapshot, err := s.pressureSnapshot(path); err == nil {
		swap = snapshot.Swap
		pressure = snapshot.MemoryPressure
	}
	findings := hostpressure.AssessProvisionStart(hostpressure.ProvisionStart{
		RunningVMMiB:   s.runningVMMemoryMiB(),
		NewVMMiB:       addMiB,
		HostFreeMiB:    freeMiB,
		HostTotalMiB:   totalMiB,
		ReserveMiB:     balloon.DefaultConfig().ReserveMiB,
		Swap:           swap,
		MemoryPressure: pressure,
	})
	return applyPressureFindings(findings, force)
}

// stoppedNodeMemoryMiB sums the configured memory of the cluster's nodes that
// are not running: exactly what a start is about to commit. A fully stopped
// cluster sums to its whole configured memory; a partly-running one to the half
// that is still to boot.
func (s *Server) stoppedNodeMemoryMiB(item cluster.Cluster) int {
	total := 0
	for _, node := range item.Nodes {
		if !s.nodeRunning(item.Name, node.Name) {
			total += item.DefaultsFor(node.Role).MemoryMiB
		}
	}
	return total
}

// runningVMMemoryMiB sums the configured memory of the guests that are actually
// running, node by node — the mirror image of stoppedNodeMemoryMiB, and for the
// same reason. Counting a partly-running cluster's whole configured memory
// would let a `node start` refusal claim more resident memory than the host
// holds, and this repo's refusals are numbers an operator can re-derive.
// Configured — not observed — memory is the right unit: it is what a booting
// guest eventually claims, and what checkOvercommit accounts in.
func (s *Server) runningVMMemoryMiB() int {
	clusters, err := cluster.List()
	if err != nil {
		return 0
	}
	total := 0
	for _, item := range clusters {
		for _, node := range item.Nodes {
			if s.nodeRunning(item.Name, node.Name) {
				total += item.DefaultsFor(node.Role).MemoryMiB
			}
		}
	}
	return total
}

func (s *Server) pressureSnapshot(path string) (hostpressure.Snapshot, error) {
	measure := s.hostPressure
	if measure == nil {
		measure = measureHostPressure
	}
	return measure(path)
}

// applyPressureFindings turns a classification into the operation's refusal or
// warnings: blocking findings refuse unless forced, advisory ones always ride
// along, and each finding stays its own warning so the CLI prints one per line.
func applyPressureFindings(findings []hostpressure.Finding, force bool) ([]string, error) {
	var blocking, advisory []string
	for _, finding := range findings {
		if finding.Severity == hostpressure.SeverityBlock {
			blocking = append(blocking, finding.String())
			continue
		}
		advisory = append(advisory, finding.String())
	}
	if len(blocking) == 0 {
		return advisory, nil
	}
	if !force {
		return nil, fmt.Errorf("%s (use --force to override)", strings.Join(blocking, "; "))
	}
	for i := range blocking {
		blocking[i] += " (forced)"
	}
	return append(blocking, advisory...), nil
}

func (s *Server) checkLonghornMemoryWarning(item cluster.Cluster) string {
	if item.CSI != cluster.CSILonghorn {
		return ""
	}
	measure := s.hostFreeMemory
	if measure == nil {
		measure = balloon.HostFreeMiB
	}
	freeMiB, err := measure()
	if err != nil {
		return ""
	}
	reserveMiB := balloon.DefaultConfig().ReserveMiB
	projectedFreeMiB := freeMiB - clusterMemoryMiB(item)
	if projectedFreeMiB >= reserveMiB {
		return ""
	}
	if projectedFreeMiB < 0 {
		projectedFreeMiB = 0
	}
	return fmt.Sprintf("Longhorn on a memory-tight host: projected free memory %d MiB is below the %d MiB balloon reserve; storage replicas may stall under swap pressure, but tbx will continue", projectedFreeMiB, reserveMiB)
}

func clusterMemoryMiB(item cluster.Cluster) int {
	total := 0
	for _, node := range item.Nodes {
		total += item.DefaultsFor(node.Role).MemoryMiB
	}
	return total
}
