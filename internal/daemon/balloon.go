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
	currentTargetMiB        int
	tolerateDeviceNotActive bool
	// recordTarget publishes a target the guest actually accepted, so the next
	// pre-balloon measures what is left rather than assuming the guest is still
	// at its configured size. Nil on a machine built outside the server.
	recordTarget func(int)
}

func (m balloonMachine) ConfiguredMiB() int { return m.configuredMiB }

// CurrentTargetMiB is the last balloon target this daemon applied to the guest,
// falling back to the configured size for a guest nothing has ballooned yet.
func (m balloonMachine) CurrentTargetMiB() int {
	if m.currentTargetMiB > 0 {
		return m.currentTargetMiB
	}
	return m.configuredMiB
}

func (m balloonMachine) SetMemoryTargetMiB(targetMiB int) error {
	err := m.machine.SetMemoryTargetMiB(targetMiB)
	if err != nil {
		// A tolerated inactive balloon device is not a failure, but it is also
		// not a guest that gave memory back: the driver is not loaded yet, so
		// the target goes unrecorded and the node still counts as reclaimable.
		if m.tolerateDeviceNotActive && errors.Is(err, hypervisor.ErrDeviceNotActive) {
			return nil
		}
		return err
	}
	if m.recordTarget != nil {
		m.recordTarget(targetMiB)
	}
	return nil
}

// balloonCurrent is the optional current-target readback a Balloonable may
// carry. The balloon package's interface is deliberately configured-size only —
// its reconcile is anchored there — so the pre-balloon asks for the reading it
// needs without forcing every implementation to have one.
type balloonCurrent interface {
	CurrentTargetMiB() int
}

// currentTargetMiB is how much memory a running guest holds right now as far as
// this daemon knows: the last target it applied, or the configured size for a
// guest nothing has ballooned.
func currentTargetMiB(vm balloon.Balloonable) int {
	if current, ok := vm.(balloonCurrent); ok {
		if target := current.CurrentTargetMiB(); target > 0 {
			return target
		}
	}
	return vm.ConfiguredMiB()
}

// balloonCandidate is one running node as the balloon controller sees it,
// captured from the VM inventory so the apid probe that decides eligibility can
// run without holding opMu.
type balloonCandidate struct {
	key                     string
	machine                 hypervisor.Machine
	configuredMiB           int
	currentTargetMiB        int
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
	return balloonablesFrom(candidates, balloonReadback, s.recordBalloonTarget)
}

// balloonablesLocked is Balloonables for a caller that already holds opMu —
// which every guest-start gate does, since the whole provisioning dispatch runs
// under it. Taking opMu again there would deadlock, so the lock stays with the
// caller and only the apid probe (which Balloonables runs outside the lock)
// moves inside it. The gate calls this only once it has already computed a
// shortfall against the measured host, so the roomy-host path never probes, and
// the probe is bounded by its own dial timeout.
func (s *Server) balloonablesLocked() map[string]balloon.Balloonable {
	balloonReadback := s.hypervisor.Capabilities().BalloonReadback.Supported
	return balloonablesFrom(s.balloonCandidatesLocked(balloonReadback), balloonReadback, s.recordBalloonTarget)
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
			key := clusterName + "/" + nodeName
			candidates[key] = balloonCandidate{
				key:                     key,
				machine:                 machine,
				configuredMiB:           item.DefaultsFor(node.Role).MemoryMiB,
				currentTargetMiB:        s.balloonTargetMiB(key),
				ip:                      cluster.LookupIP(node.MAC, item.SubnetIndex),
				tolerateDeviceNotActive: balloonReadback,
			}
		}
	}
	// A node that is no longer running boots at its configured size when it
	// comes back, so its recorded target must not outlive it.
	s.pruneBalloonTargets(candidates)
	return candidates
}

// balloonablesFrom applies the eligibility rule to the captured candidates.
func balloonablesFrom(candidates map[string]balloonCandidate, balloonReadback bool, record func(string, int)) map[string]balloon.Balloonable {
	out := map[string]balloon.Balloonable{}
	for key, e := range candidates {
		if balloonReadback || (e.ip != "" && ClassifyPhase(true, probeAPID(e.ip)) == PhaseConfigured) {
			machine := balloonMachine{
				machine:                 e.machine,
				configuredMiB:           e.configuredMiB,
				currentTargetMiB:        e.currentTargetMiB,
				tolerateDeviceNotActive: e.tolerateDeviceNotActive,
			}
			if record != nil {
				machine.recordTarget = func(targetMiB int) { record(key, targetMiB) }
			}
			out[key] = machine
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
//
// The returned heldMiB is the pre-balloon this admission took, for the caller
// to re-arm with holdBalloonReclaim at the moment it actually launches the
// guests: the hold is a boot-window budget, so its clock must not start while
// work the launch still has to do (a cold image fetch, disk clones) is in
// front of it. Callers that launch immediately can ignore it.
func (s *Server) checkProvisionStart(path string, addMiB int, force bool) ([]string, int, error) {
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
	in := hostpressure.ProvisionStart{
		RunningVMMiB:   runningMiB,
		NewVMMiB:       addMiB,
		HostFreeMiB:    freeMiB,
		HostTotalMiB:   totalMiB,
		ReserveMiB:     reserveMiB,
		Swap:           swap,
		MemoryPressure: pressure,
	}
	// The balloon credit is measured only when the host is actually short.
	// Measuring it dials apid on every running node on backends without balloon
	// readback — seconds, on a host whose siblings are mid-bringup — and this
	// gate runs under opMu, so the roomy-host path must never pay for a credit
	// no shortfall will ever spend.
	var reclaim balloonReclaim
	if hostpressure.ProvisionStartShortfallMiB(in) > 0 {
		reclaim = s.balloonReclaim()
		in.ReclaimableMiB = reclaim.availableMiB
	}
	plan := hostpressure.AssessProvisionStart(in)
	warnings, err := applyPressureFindings(plan.Findings, force)
	if err != nil {
		return nil, 0, err
	}
	if plan.ReclaimMiB == 0 {
		return warnings, 0, nil
	}
	heldMiB, err := reclaim.apply(plan.ReclaimMiB)
	if err != nil {
		// Memory nothing gave back is not headroom. The gate admitted this
		// start only because the reclaim was going to happen, so a reclaim that
		// did not happen puts it back where an unaided host would be.
		detail := fmt.Sprintf("pre-ballooning %d MiB out of the %d MiB of guests already running failed: %v", plan.ReclaimMiB, runningMiB, err)
		if !force {
			return nil, 0, fmt.Errorf("%s; %s (use --force to override)", detail, hostpressure.MemoryRemedy)
		}
		return append(warnings, detail+" (forced)"), 0, nil
	}
	s.holdBalloonReclaim(heldMiB)
	return append(warnings, fmt.Sprintf(
		"pre-ballooned %d MiB out of the %d MiB of guests already running so the %d MiB starting now still leaves the %d MiB balloon reserve free of the %d MiB measured;"+
			" the balloon controller holds that reclaim for %s while the new guests boot, then hands it back",
		plan.ReclaimMiB, runningMiB, addMiB, reserveMiB, freeMiB, balloonReclaimHoldTTL,
	)), heldMiB, nil
}

// balloonReclaimHoldTTL is how long a pre-balloon taken at admission is held
// against the balloon manager's own reconcile. Without a hold the manager
// undoes the reclaim at its next poll: it sees free memory back above the
// reserve — because the reclaim just put it there — computes no deficit, and
// deflates every guest to its configured size in the seconds before the new
// guest has claimed anything, which is exactly the concurrent-bringup squeeze
// the gate admitted this start to avoid. The hold covers that whole window
// because it keeps the deficit at the reclaim regardless of what the host-free
// reading does while the guest boots and claims its allocation. The hold outlasts a Talos boot
// (~90s to virtio_balloon) with margin, and expires on its own so a start that
// never happened cannot pin guest memory down forever.
const balloonReclaimHoldTTL = 5 * time.Minute

// balloonHold is one start's pre-balloon: the memory it took out of the running
// guests, measured from their configured size, and the deadline its boot window
// runs to. Holds are per start so a start that never launched can drop its own
// without touching an overlapping start's.
type balloonHold struct {
	reclaimMiB int
	until      time.Time
}

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
// Callers must reach this only when the headroom rule is actually short (see
// checkProvisionStart): Balloonables dials apid on backends without balloon
// readback, which is too expensive to spend on a host that has the headroom
// outright. The cheap inventory ceiling then short-circuits the rest when
// nothing running sits above the per-node floor.
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
		// The current target, not the configured size: memory the balloon
		// manager has already reclaimed is in the host-free reading the gate
		// measured, so counting it again here would credit it twice and admit a
		// start on headroom that does not exist.
		if above := currentTargetMiB(vm) - floorMiB; above > 0 {
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
// can never disagree about where the memory comes from. The plan is anchored on
// each guest's *current* target rather than its configured size, so a
// pre-balloon can only ever take memory back — anchoring on configured would
// hand memory to guests the manager had already shrunk, lowering host free
// memory in the moment the gate promised to raise it.
func (r balloonReclaim) apply(deficitMiB int) (int, error) {
	names := make([]string, 0, len(r.vms))
	nodes := make([]balloon.Node, 0, len(r.vms))
	for name, vm := range r.vms {
		names = append(names, name)
		nodes = append(nodes, balloon.Node{Name: name, ConfiguredMiB: currentTargetMiB(vm)})
	}
	sort.Strings(names)
	// PlanTargets drops the sub-MiB residual of its proportional split so a
	// reconcile never over-reclaims; a gate that promised the reserve exactly
	// would then land a few MiB short of what it admitted the start on. Asking
	// for one MiB per node more absorbs that residual and is itself far below
	// the granularity of any of these readings.
	targets := balloon.PlanTargets(nodes, deficitMiB+len(nodes), r.floorMiB)
	heldMiB := 0
	for _, name := range names {
		if err := r.vms[name].SetMemoryTargetMiB(targets[name]); err != nil {
			return 0, fmt.Errorf("balloon %s down to %d MiB: %w", name, targets[name], err)
		}
		// The hold is measured from the CONFIGURED size, not from the deficit
		// this call asked for: the balloon manager's reconcile anchors on
		// configured, so a hold that only covered the increment would let the
		// next poll inflate the guests back past everything an earlier reclaim
		// had already taken.
		if out := r.vms[name].ConfiguredMiB() - targets[name]; out > 0 {
			heldMiB += out
		}
	}
	return heldMiB, nil
}

// holdBalloonReclaim records a pre-balloon so the balloon manager keeps it in
// place while the admitted guests boot. reclaimMiB is the memory now out of the
// guests measured from their configured size — the same anchor the manager's
// reconcile uses — so overlapping starts hold the largest outstanding reclaim
// rather than the sum: the second start's reclaim already contains the first's,
// because it was applied on top of it.
//
// Each call records its own entry rather than raising a shared scalar, so a
// start that fails can drop exactly the hold it took and leave an overlapping
// start's hold standing (see releaseBalloonHold).
func (s *Server) holdBalloonReclaim(reclaimMiB int) {
	if reclaimMiB <= 0 {
		return
	}
	s.balloonHoldMu.Lock()
	defer s.balloonHoldMu.Unlock()
	now := time.Now()
	s.dropExpiredBalloonHoldsLocked(now)
	s.balloonHolds = append(s.balloonHolds, balloonHold{reclaimMiB: reclaimMiB, until: now.Add(balloonReclaimHoldTTL)})
}

// releaseBalloonHold drops a hold armed at admission for a start that never
// launched — every failure between the gate and the launch (a domain clash, an
// image fetch, a disk clone) would otherwise leave memory held out of the
// running guests for the whole TTL on behalf of guests that never booted. Only
// this start's own entry goes: a hold taken by an overlapping start stays,
// because it belongs to that start's still-booting guests, not to this one.
func (s *Server) releaseBalloonHold(reclaimMiB int) {
	if reclaimMiB <= 0 {
		return
	}
	s.balloonHoldMu.Lock()
	defer s.balloonHoldMu.Unlock()
	s.dropExpiredBalloonHoldsLocked(time.Now())
	// Newest first: operations are serialized under opMu, so the most recent
	// entry of this size is the one this start armed.
	for i := len(s.balloonHolds) - 1; i >= 0; i-- {
		if s.balloonHolds[i].reclaimMiB == reclaimMiB {
			s.balloonHolds = append(s.balloonHolds[:i], s.balloonHolds[i+1:]...)
			return
		}
	}
}

// dropExpiredBalloonHoldsLocked forgets holds whose boot window has run out.
func (s *Server) dropExpiredBalloonHoldsLocked(now time.Time) {
	live := s.balloonHolds[:0]
	for _, hold := range s.balloonHolds {
		if now.Before(hold.until) {
			live = append(live, hold)
		}
	}
	s.balloonHolds = live
}

// BalloonHoldMiB is the outstanding pre-balloon the manager must not hand back
// yet. The manager treats it as a floor on its reconcile deficit, which holds
// the guests at their reclaimed targets and needs no extra state in the balloon
// package. It cannot be a debit against the host-free reading: the reclaim is
// already in that reading, so subtracting it reproduces the pre-reclaim number
// and the very next poll deflates every guest back to configured.
//
// The hold is clamped to what the running guests *could* give back — the same
// inventory ceiling the gate credits, sum(configured − floor) over running
// nodes — because the deficit it manufactures is spread over whatever is
// balloonable at that tick. Without the clamp, stopping or destroying the
// reclaimed cluster inside the window would redirect the whole deficit onto the
// newly started one and pin it at the floor on a host with memory to spare. A
// hold can therefore never outlive the guests it was taken from. The clamp is
// against the ceiling and not against what is *currently* out, because the
// manager may legitimately have handed a reclaim back (an expired hold that a
// slow create then re-armed at its launch), and the hold is precisely the
// instruction to take it again. It is also non-destructive: a clamped reading
// never writes the lowered value back, so one transient inventory read cannot
// disarm a live hold for good.
func (s *Server) BalloonHoldMiB() int {
	if !s.balloonHoldArmed() {
		return 0
	}
	// Measured before balloonHoldMu is taken: the admission path holds opMu
	// while it arms the hold, so taking opMu under balloonHoldMu would invert
	// the lock order.
	ceiling := s.balloonReclaimCeilingMiB()
	s.balloonHoldMu.Lock()
	defer s.balloonHoldMu.Unlock()
	s.dropExpiredBalloonHoldsLocked(time.Now())
	held := 0
	for _, hold := range s.balloonHolds {
		if hold.reclaimMiB > held {
			held = hold.reclaimMiB
		}
	}
	if held > ceiling {
		return ceiling
	}
	return held
}

// balloonHoldArmed is the cheap check that keeps a poll on an idle daemon from
// paying for the inventory read the clamp needs.
func (s *Server) balloonHoldArmed() bool {
	s.balloonHoldMu.Lock()
	defer s.balloonHoldMu.Unlock()
	s.dropExpiredBalloonHoldsLocked(time.Now())
	return len(s.balloonHolds) > 0
}

// balloonReclaimCeilingMiB is how much the running balloonable guests could
// give back in total, read from the cluster inventory under opMu the way every
// other reader of s.vms does.
func (s *Server) balloonReclaimCeilingMiB() int {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.reclaimCeilingMiB(balloon.DefaultConfig().FloorMiB)
}

// recordBalloonTarget remembers a target a guest accepted. Both the balloon
// manager's reconcile and the gate's own pre-balloon go through the same
// balloonMachine wrapper, so every applied target lands here.
func (s *Server) recordBalloonTarget(key string, targetMiB int) {
	s.balloonTargetMu.Lock()
	defer s.balloonTargetMu.Unlock()
	if s.balloonTargets == nil {
		s.balloonTargets = map[string]int{}
	}
	s.balloonTargets[key] = targetMiB
}

// balloonTargetMiB is the last target applied to a node, or 0 for a node
// nothing has ballooned.
func (s *Server) balloonTargetMiB(key string) int {
	s.balloonTargetMu.Lock()
	defer s.balloonTargetMu.Unlock()
	return s.balloonTargets[key]
}

// pruneBalloonTargets forgets nodes that are no longer running: a node that
// stopped comes back at its configured size, and a stale target would make the
// gate think memory it never reclaimed is already gone.
func (s *Server) pruneBalloonTargets(live map[string]balloonCandidate) {
	s.balloonTargetMu.Lock()
	defer s.balloonTargetMu.Unlock()
	for key := range s.balloonTargets {
		if _, ok := live[key]; !ok {
			delete(s.balloonTargets, key)
		}
	}
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
