package daemon

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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

// balloonCandidate is one running node as the balloon controller sees it,
// captured from the VM inventory so the apid probe that decides eligibility can
// run without holding opMu.
type balloonCandidate struct {
	machine                 hypervisor.Machine
	configuredMiB           int
	ip                      string
	tolerateDeviceNotActive bool
}

// Balloonables snapshots the CONFIGURED running nodes for the balloon manager,
// reading s.vms under opMu so the manager never races an op. Backends with
// guest-visible balloon readback can manage every active node directly. On VZ
// without readback, maintenance-mode nodes remain exempt (SPEC §8): their
// guest has no virtio_balloon driver, and setting a target on one crashes vz,
// so only TLS-configured nodes observed through apid are managed.
func (s *Server) Balloonables() map[string]balloon.Balloonable {
	balloonReadback := s.hypervisor.Capabilities().BalloonReadback.Supported
	s.opMu.Lock()
	candidates := s.balloonCandidatesLocked(balloonReadback)
	s.opMu.Unlock()
	return balloonablesFrom(candidates, balloonReadback)
}

// balloonablesLocked is Balloonables for a caller that already holds opMu —
// which every guest-start gate does, since the whole provisioning dispatch runs
// under it. Taking opMu again there would deadlock, so the lock stays with the
// caller and only the apid probe (which Balloonables runs outside the lock)
// moves inside it. That probe only ever runs when the gate is short of headroom
// and is bounded by its own dial timeout.
func (s *Server) balloonablesLocked() map[string]balloon.Balloonable {
	balloonReadback := s.hypervisor.Capabilities().BalloonReadback.Supported
	return balloonablesFrom(s.balloonCandidatesLocked(balloonReadback), balloonReadback)
}

// balloonCandidatesLocked reads the running, configured nodes out of the VM
// inventory. Callers must hold opMu.
func (s *Server) balloonCandidatesLocked(balloonReadback bool) map[string]balloonCandidate {
	candidates := map[string]balloonCandidate{}
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
			candidates[clusterName+"/"+nodeName] = balloonCandidate{
				machine:                 machine,
				configuredMiB:           item.DefaultsFor(node.Role).MemoryMiB,
				ip:                      cluster.LookupIP(node.MAC, item.SubnetIndex),
				tolerateDeviceNotActive: balloonReadback,
			}
		}
	}
	return candidates
}

// balloonablesFrom applies the eligibility rule to the captured candidates.
func balloonablesFrom(candidates map[string]balloonCandidate, balloonReadback bool) map[string]balloon.Balloonable {
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
		measure = measureHostTotalMiB
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
	reserveMiB := balloon.DefaultConfig().ReserveMiB
	runningMiB := s.runningVMMemoryMiB()
	reclaim := s.balloonReclaim()
	plan := hostpressure.AssessProvisionStart(hostpressure.ProvisionStart{
		RunningVMMiB:   runningMiB,
		NewVMMiB:       addMiB,
		HostFreeMiB:    freeMiB,
		HostTotalMiB:   totalMiB,
		ReserveMiB:     reserveMiB,
		ReclaimableMiB: reclaim.availableMiB,
		Swap:           swap,
		MemoryPressure: pressure,
	})
	warnings, err := applyPressureFindings(plan.Findings, force)
	if err != nil {
		return nil, err
	}
	if plan.ReclaimMiB == 0 {
		return warnings, nil
	}
	if err := reclaim.apply(plan.ReclaimMiB); err != nil {
		// Memory nothing gave back is not headroom. The gate admitted this
		// start only because the reclaim was going to happen, so a reclaim that
		// did not happen puts it back where an unaided host would be.
		detail := fmt.Sprintf("pre-ballooning %d MiB out of the %d MiB of guests already running failed: %v", plan.ReclaimMiB, runningMiB, err)
		if !force {
			return nil, fmt.Errorf("%s; %s (use --force to override)", detail, hostpressure.MemoryRemedy)
		}
		return append(warnings, detail+" (forced)"), nil
	}
	s.holdBalloonReclaim(plan.ReclaimMiB)
	return append(warnings, fmt.Sprintf(
		"pre-ballooned %d MiB out of the %d MiB of guests already running so the %d MiB starting now still leaves the %d MiB balloon reserve free of the %d MiB measured;"+
			" the balloon controller holds that reclaim for %s while the new guests boot, then hands it back",
		plan.ReclaimMiB, runningMiB, addMiB, reserveMiB, freeMiB, balloonReclaimHoldTTL,
	)), nil
}

// balloonReclaimHoldTTL is how long a pre-balloon taken at admission is held
// against the balloon manager's own reconcile. Without a hold the manager
// undoes the reclaim at its next poll: it sees free memory back above the
// reserve — because the reclaim just put it there — computes no deficit, and
// deflates every guest to its configured size in the seconds before the new
// guest has claimed anything, which is exactly the concurrent-bringup squeeze
// the gate admitted this start to avoid. The hold outlasts a Talos boot
// (~90s to virtio_balloon) with margin, and expires on its own so a start that
// never happened cannot pin guest memory down forever.
const balloonReclaimHoldTTL = 5 * time.Minute

// balloonReclaim is what the balloon controller can hand back right now: the
// running guests it can shrink, how much host memory that is worth, and the
// per-node floor it must not cross.
type balloonReclaim struct {
	vms          map[string]balloon.Balloonable
	availableMiB int
	floorMiB     int
}

// balloonReclaim measures the memory already-running guests can give back
// before a new one boots (#398). Memory a *running* guest holds is not the same
// resource as the allocation a booting guest is about to claim: the first can be
// reclaimed on demand through virtio_balloon, the second cannot be reclaimed at
// all until the guest finishes booting. Counting the two the same way is what
// made the gate terminal — an 81 MiB shortfall stood between a host and a start
// it could have made room for.
//
// The cheap ceiling runs first so the common path never probes: with nothing
// running above the per-node floor there is nothing to reclaim, and Balloonables
// — which dials apid on backends without balloon readback — is never called.
func (s *Server) balloonReclaim() balloonReclaim {
	floorMiB := balloon.DefaultConfig().FloorMiB
	if s.reclaimCeilingMiB(floorMiB) <= 0 {
		return balloonReclaim{floorMiB: floorMiB}
	}
	list := s.balloonables
	if list == nil {
		list = s.balloonablesLocked
	}
	vms := list()
	availableMiB := 0
	for _, vm := range vms {
		if above := vm.ConfiguredMiB() - floorMiB; above > 0 {
			availableMiB += above
		}
	}
	return balloonReclaim{vms: vms, availableMiB: availableMiB, floorMiB: floorMiB}
}

// reclaimCeilingMiB is the upper bound on reclaimable memory read from the
// cluster inventory alone — the same node-by-node accounting runningVMMemoryMiB
// uses, so the gate's two memory numbers are always derived from one view of
// what is running.
func (s *Server) reclaimCeilingMiB(floorMiB int) int {
	clusters, err := cluster.List()
	if err != nil {
		return 0
	}
	total := 0
	for _, item := range clusters {
		for _, node := range item.Nodes {
			if !s.nodeRunning(item.Name, node.Name) {
				continue
			}
			if above := item.DefaultsFor(node.Role).MemoryMiB - floorMiB; above > 0 {
				total += above
			}
		}
	}
	return total
}

// apply asks the running guests for deficitMiB, sharing it out with the same
// water-filling plan the balloon manager uses so a pre-balloon and a reconcile
// can never disagree about where the memory comes from.
func (r balloonReclaim) apply(deficitMiB int) error {
	names := make([]string, 0, len(r.vms))
	nodes := make([]balloon.Node, 0, len(r.vms))
	for name, vm := range r.vms {
		names = append(names, name)
		nodes = append(nodes, balloon.Node{Name: name, ConfiguredMiB: vm.ConfiguredMiB()})
	}
	sort.Strings(names)
	// PlanTargets drops the sub-MiB residual of its proportional split so a
	// reconcile never over-reclaims; a gate that promised the reserve exactly
	// would then land a few MiB short of what it admitted the start on. Asking
	// for one MiB per node more absorbs that residual and is itself far below
	// the granularity of any of these readings.
	targets := balloon.PlanTargets(nodes, deficitMiB+len(nodes), r.floorMiB)
	for _, name := range names {
		if err := r.vms[name].SetMemoryTargetMiB(targets[name]); err != nil {
			return fmt.Errorf("balloon %s down to %d MiB: %w", name, targets[name], err)
		}
	}
	return nil
}

// holdBalloonReclaim records a pre-balloon so the balloon manager keeps it in
// place while the admitted guests boot. Overlapping starts hold the largest
// outstanding reclaim, not the sum: each was measured against the same host.
func (s *Server) holdBalloonReclaim(reclaimMiB int) {
	if reclaimMiB <= 0 {
		return
	}
	s.balloonHoldMu.Lock()
	defer s.balloonHoldMu.Unlock()
	if time.Now().After(s.balloonHoldUntil) {
		s.balloonHoldMiB = 0
	}
	if reclaimMiB > s.balloonHoldMiB {
		s.balloonHoldMiB = reclaimMiB
	}
	s.balloonHoldUntil = time.Now().Add(balloonReclaimHoldTTL)
}

// BalloonHoldMiB is the outstanding pre-balloon the manager must not hand back
// yet. The manager subtracts it from its host-free reading, which is the same
// arithmetic as holding the guests at their reclaimed targets and needs no extra
// state in the balloon package.
func (s *Server) BalloonHoldMiB() int {
	s.balloonHoldMu.Lock()
	defer s.balloonHoldMu.Unlock()
	if s.balloonHoldMiB == 0 {
		return 0
	}
	if time.Now().After(s.balloonHoldUntil) {
		s.balloonHoldMiB = 0
	}
	return s.balloonHoldMiB
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
		measure = measureHostFreeMiB
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
