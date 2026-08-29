package balloon

import (
	"context"
	"errors"
	"log"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/randax/talos-box/internal/hostmem"
)

const (
	defaultDeadbandMiB         = 256
	defaultMinRetargetInterval = time.Minute
	pressureArmSwapPercent     = 80
	pressureClearSwapPercent   = 70
	pressureArmCompressorPct   = 20
	pressureClearCompressorPct = 15
	pressureArmCompressorMiB   = 4 * 1024
	pressureClearCompressorMiB = 3 * 1024
)

// ErrTargetPending means the hypervisor accepted the request path, but the
// guest balloon device is not active yet and therefore did not apply it.
var ErrTargetPending = errors.New("balloon target pending: device not active")

// Balloonable is the balloon manager's view of a running configured VM.
type Balloonable interface {
	ConfiguredMiB() int
	SetMemoryTargetMiB(int) error
}

// CurrentTargeter optionally reports the guest's current balloon target.
type CurrentTargeter interface {
	CurrentTargetMiB() int
}

// Logger sinks the manager's telemetry; it has log.Printf's shape so the
// daemon's default logger (tbxd.log) can be passed straight through.
type Logger func(format string, v ...any)

// Manager applies balloon targets and emits change-driven telemetry: one line
// per node whose applied target moved since the previous reconcile. That keeps
// tbxd.log an attestation source for ballooning without the per-tick spam of
// logging every poll (#306).
type Manager struct {
	log                 Logger
	last                map[string]int
	lastAppliedDeficit  int
	lastAppliedKnown    bool
	lastAppliedHold     bool
	lastRetarget        map[string]time.Time
	retryPending        map[string]bool
	pendingLogged       map[string]bool
	devicePending       map[string]bool
	pressureLatched     bool
	deadbandMiB         int
	minRetargetInterval time.Duration
}

// NewManager returns a manager logging through logger; a nil logger uses the
// standard logger, which in tbxd is tbxd.log.
func NewManager(logger Logger) *Manager {
	if logger == nil {
		logger = log.Printf
	}
	return &Manager{
		log:                 logger,
		last:                map[string]int{},
		lastRetarget:        map[string]time.Time{},
		retryPending:        map[string]bool{},
		pendingLogged:       map[string]bool{},
		devicePending:       map[string]bool{},
		deadbandMiB:         defaultDeadbandMiB,
		minRetargetInterval: defaultMinRetargetInterval,
	}
}

// ReconcileSnapshot applies the steady-state policy to one shared memory
// sample. Pressure signals veto release of existing reclaim; they never create
// reclaim without a real available-memory deficit.
//
// A guest whose balloon device is not active yet cannot give memory back, so
// it is planned around: the deficit is shared out over the guests that can,
// and the pending guest is only probed on the retry window until its device
// comes up. Reductions are applied before releases so a pending device is
// discovered before any other guest is handed memory on its behalf; when one
// is discovered mid-tick the plan is redone without it.
func (m *Manager) ReconcileSnapshot(vms map[string]Balloonable, sample hostmem.Snapshot, reserveMiB, floorMiB, holdMiB int, now time.Time) {
	m.updatePressureLatch(sample)
	naturalDeficit := reserveMiB - sample.AvailableMiB
	if naturalDeficit < 0 {
		naturalDeficit = 0
	}
	if naturalDeficit > 0 && naturalDeficit < m.deadbandMiB {
		naturalDeficit = 0
	}
	deficit := naturalDeficit
	if holdMiB > deficit {
		deficit = holdMiB
	}
	holdSetsDeficit := holdMiB > 0 && holdMiB >= naturalDeficit

	m.probePendingDevices(vms, now)

	appliedThisTick := map[string]int{}
	anySucceeded := false
	for round := 0; round <= len(vms); round++ {
		targets := m.planActive(vms, deficit, floorMiB)
		changed, hasUnappliedTarget := m.classify(vms, targets)
		if !changed {
			break
		}
		if round == 0 && !holdSetsDeficit && !m.lastAppliedHold && m.lastAppliedKnown && absInt(deficit-m.lastAppliedDeficit) < m.deadbandMiB && !hasUnappliedTarget {
			break
		}
		names := make([]string, 0, len(targets))
		for name := range targets {
			names = append(names, name)
		}
		sort.Strings(names)
		newlyPending := false
		for _, reductions := range []bool{true, false} {
			for _, name := range names {
				target := targets[name]
				if (target < m.currentMiB(vms[name], name)) != reductions {
					continue
				}
				if prior, ok := appliedThisTick[name]; ok && target >= prior {
					// Already moved this tick; a replan never hands back
					// what this tick just took.
					continue
				}
				outcome := m.applyTarget(vms, name, target, holdSetsDeficit, sample, reserveMiB, deficit, now)
				switch outcome {
				case applyOutcomeApplied:
					appliedThisTick[name] = target
					anySucceeded = true
				case applyOutcomePending:
					newlyPending = true
				}
			}
			if newlyPending {
				break
			}
		}
		if !newlyPending {
			break
		}
	}
	if anySucceeded {
		m.lastAppliedDeficit = deficit
		m.lastAppliedKnown = true
		m.lastAppliedHold = holdSetsDeficit
	}
	m.dropDeparted(vms)
}

type applyOutcome int

const (
	applyOutcomeSkipped applyOutcome = iota
	applyOutcomeApplied
	applyOutcomePending
	applyOutcomeFailed
)

// applyTarget is one node's share of a tick: the change-driven skip, the
// per-node retry window, and the telemetry for whatever the guest answered.
func (m *Manager) applyTarget(vms map[string]Balloonable, name string, target int, holdSetsDeficit bool, sample hostmem.Snapshot, reserveMiB, deficit int, now time.Time) applyOutcome {
	previous, known := m.last[name]
	externalTarget, externallyMoved := externallyMovedTarget(vms[name], previous, known)
	if externallyMoved {
		known = false
	}
	if known && previous == target {
		return applyOutcomeSkipped
	}
	if !holdSetsDeficit && (!externallyMoved || m.retryPending[name]) {
		if last, ok := m.lastRetarget[name]; ok && now.Sub(last) < m.minRetargetInterval {
			m.retryPending[name] = true
			return applyOutcomeSkipped
		}
	}
	if err := vms[name].SetMemoryTargetMiB(target); err != nil {
		if errors.Is(err, ErrTargetPending) {
			if !m.pendingLogged[name] {
				m.log("balloon %s: %v", name, err)
				m.pendingLogged[name] = true
			}
			m.devicePending[name] = true
			m.retryPending[name] = true
			m.lastRetarget[name] = now
			return applyOutcomePending
		}
		m.log("balloon %s: %v", name, err)
		m.retryPending[name] = true
		return applyOutcomeFailed
	}
	m.last[name] = target
	m.lastRetarget[name] = now
	delete(m.retryPending, name)
	delete(m.pendingLogged, name)
	if externallyMoved {
		m.log("balloon %s: target=%dMiB (restoring externally moved target %d)", name, target, externalTarget)
		return applyOutcomeApplied
	}
	m.log("balloon %s: target=%dMiB (configured=%d hostFree=%d reserve=%d deficit=%d compressor=%dMiB swapUsed=%d%% pressureLatched=%t)",
		name, target, vms[name].ConfiguredMiB(), sample.AvailableMiB, reserveMiB, deficit,
		sample.CompressorMiB, snapshotSwapPercent(sample), m.pressureLatched)
	return applyOutcomeApplied
}

// planActive shares deficit over the guests whose balloon device answers,
// clamped under a pressure latch so no guest is handed memory back.
func (m *Manager) planActive(vms map[string]Balloonable, deficit, floorMiB int) map[string]int {
	nodes := make([]Node, 0, len(vms))
	for name, vm := range vms {
		if m.devicePending[name] {
			continue
		}
		nodes = append(nodes, Node{Name: name, ConfiguredMiB: vm.ConfiguredMiB()})
	}
	targets := PlanTargets(nodes, deficit, floorMiB)
	if m.pressureLatched {
		for name, target := range targets {
			if previous, ok := m.last[name]; ok {
				ceiling := previous
				if current, ok := vms[name].(CurrentTargeter); ok {
					if currentTarget := current.CurrentTargetMiB(); currentTarget > 0 && currentTarget < ceiling {
						ceiling = currentTarget
					}
				}
				if target > ceiling {
					targets[name] = ceiling
				}
			}
		}
	}
	return targets
}

// classify reports whether any planned target differs from the manager's
// record, and whether one of those is a node with no applied target yet.
func (m *Manager) classify(vms map[string]Balloonable, targets map[string]int) (changed, hasUnappliedTarget bool) {
	for name, target := range targets {
		previous, known := m.last[name]
		if _, moved := externallyMovedTarget(vms[name], previous, known); moved {
			known = false
		}
		if !known || previous != target {
			changed = true
			if !known || m.retryPending[name] {
				hasUnappliedTarget = true
			}
		} else {
			delete(m.retryPending, name)
		}
	}
	return changed, hasUnappliedTarget
}

// currentMiB is what the guest holds as far as the manager can tell: its
// reported current target, else the manager's record, else its configured size.
func (m *Manager) currentMiB(vm Balloonable, name string) int {
	if current, ok := vm.(CurrentTargeter); ok {
		if target := current.CurrentTargetMiB(); target > 0 {
			return target
		}
	}
	if previous, ok := m.last[name]; ok {
		return previous
	}
	return vm.ConfiguredMiB()
}

// probePendingDevices asks each guest with an inactive balloon device for its
// configured size once per retry window — a no-op for the guest, which never
// inflated, that tells the manager when the device has come up. A guest whose
// device answers rejoins the plan on this tick.
func (m *Manager) probePendingDevices(vms map[string]Balloonable, now time.Time) {
	for name := range m.devicePending {
		vm, ok := vms[name]
		if !ok {
			continue
		}
		if last, ok := m.lastRetarget[name]; ok && now.Sub(last) < m.minRetargetInterval {
			continue
		}
		err := vm.SetMemoryTargetMiB(vm.ConfiguredMiB())
		if err != nil {
			if !errors.Is(err, ErrTargetPending) {
				m.log("balloon %s: %v", name, err)
			}
			m.lastRetarget[name] = now
			continue
		}
		delete(m.devicePending, name)
		delete(m.retryPending, name)
		delete(m.pendingLogged, name)
		m.last[name] = vm.ConfiguredMiB()
		m.log("balloon %s: device active; target=%dMiB", name, vm.ConfiguredMiB())
	}
}

func externallyMovedTarget(vm Balloonable, previous int, known bool) (int, bool) {
	if !known {
		return 0, false
	}
	current, ok := vm.(CurrentTargeter)
	if !ok {
		return 0, false
	}
	target := current.CurrentTargetMiB()
	return target, target != previous
}

func (m *Manager) updatePressureLatch(sample hostmem.Snapshot) {
	swapPercent := snapshotSwapPercent(sample)
	compressorArmed := sample.CompressorMiB >= pressureArmCompressorMiB
	compressorClear := sample.CompressorMiB < pressureClearCompressorMiB
	if sample.TotalMiB > 0 {
		compressorArmed = sample.CompressorMiB*100 >= sample.TotalMiB*pressureArmCompressorPct
		compressorClear = sample.CompressorMiB*100 < sample.TotalMiB*pressureClearCompressorPct
	}
	if swapPercent >= pressureArmSwapPercent || compressorArmed || sample.Pressure == hostmem.PressureWarning || sample.Pressure == hostmem.PressureCritical {
		m.pressureLatched = true
		return
	}
	if swapPercent < pressureClearSwapPercent && compressorClear && sample.Pressure != hostmem.PressureWarning && sample.Pressure != hostmem.PressureCritical {
		m.pressureLatched = false
	}
}

func snapshotSwapPercent(sample hostmem.Snapshot) int {
	if sample.SwapTotalBytes == 0 || sample.SwapAvailableBytes >= sample.SwapTotalBytes {
		return 0
	}
	return int((sample.SwapTotalBytes - sample.SwapAvailableBytes) * 100 / sample.SwapTotalBytes)
}

func (m *Manager) dropDeparted(vms map[string]Balloonable) {
	for name := range m.last {
		if _, ok := vms[name]; !ok {
			delete(m.last, name)
			delete(m.lastRetarget, name)
		}
	}
	for name := range m.lastRetarget {
		if _, ok := vms[name]; !ok {
			delete(m.lastRetarget, name)
			delete(m.retryPending, name)
		}
	}
	for name := range m.retryPending {
		if _, ok := vms[name]; !ok {
			delete(m.retryPending, name)
		}
	}
	for name := range m.pendingLogged {
		if _, ok := vms[name]; !ok {
			delete(m.pendingLogged, name)
		}
	}
	for name := range m.devicePending {
		if _, ok := vms[name]; !ok {
			delete(m.devicePending, name)
		}
	}
	if len(m.last) == 0 {
		m.lastAppliedDeficit = 0
		m.lastAppliedKnown = false
		m.lastAppliedHold = false
	}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// Reconcile computes and applies balloon targets for one poll: if host free
// memory is below the reserve, inflate proportionally to reclaim the deficit
// (never below floorMiB); otherwise deflate every node back to configured.
//
// This is the stateless entry point — it logs every adjustment. Long-running
// callers should use Manager.Reconcile, which only logs changes.
func Reconcile(vms map[string]Balloonable, hostFreeMiB, reserveMiB, floorMiB int) {
	NewManager(nil).Reconcile(vms, hostFreeMiB, reserveMiB, floorMiB)
}

// Reconcile applies one poll's targets and logs the ones that changed.
func (m *Manager) Reconcile(vms map[string]Balloonable, hostFreeMiB, reserveMiB, floorMiB int) {
	names := make([]string, 0, len(vms))
	nodes := make([]Node, 0, len(vms))
	for name, v := range vms {
		names = append(names, name)
		nodes = append(nodes, Node{Name: name, ConfiguredMiB: v.ConfiguredMiB()})
	}
	sort.Strings(names)

	deficit := reserveMiB - hostFreeMiB
	if deficit < 0 {
		deficit = 0
	}
	targets := PlanTargets(nodes, deficit, floorMiB)
	for _, name := range names {
		target := targets[name]
		if err := vms[name].SetMemoryTargetMiB(target); err != nil {
			m.log("balloon %s: %v", name, err)
			// Forget the node so its target is re-logged once it recovers.
			delete(m.last, name)
			continue
		}
		if prev, ok := m.last[name]; !ok || prev != target {
			m.log("balloon %s: target=%dMiB (configured=%d hostFree=%d reserve=%d deficit=%d)",
				name, target, vms[name].ConfiguredMiB(), hostFreeMiB, reserveMiB, deficit)
		}
		m.last[name] = target
	}
	// Drop departed nodes so a returning node logs its target again.
	for name := range m.last {
		if _, ok := vms[name]; !ok {
			delete(m.last, name)
		}
	}
}

// Config holds the manager's tuning (SPEC §8 / gate G3 defaults).
type Config struct {
	ReserveMiB          int
	FloorMiB            int
	PollInterval        time.Duration
	DeadbandMiB         int
	MinRetargetInterval time.Duration
	// HoldMiB reports memory that has already been ballooned out of the running
	// guests on purpose and must stay out — the pre-balloon the provision-start
	// gate takes to make room for a guest that is booting (#398). It is a floor
	// on the reconcile's deficit, not a debit against the host-free reading:
	// the reclaim is *already* in that reading, so subtracting it just
	// reproduces the pre-reclaim number and the manager hands every guest
	// straight back to its configured size on the next poll — the concurrent
	// bringup squeeze the pre-balloon exists to prevent. Nil means nothing is
	// held.
	HoldMiB func() int
	// HostFreeMiB is the host-memory probe the poll loop reads. Nil means the
	// platform probe (HostFreeMiB). It is a seam so a build that *has* a probe
	// can still exercise the platform that has none (#446).
	HostFreeMiB func() (int, error)
	// HostMemory is the shared host-memory probe. HostFreeMiB remains as a
	// compatibility seam for older callers and tests.
	HostMemory func(context.Context) (hostmem.Snapshot, error)
	Now        func() time.Time
}

// DefaultConfig is the G3-tuned default: 6 GiB host reserve, 1 GiB per-node
// floor, polled every 5s. TBX_BALLOON_RESERVE_MIB overrides the reserve for
// tuning on hosts with more/less RAM.
func DefaultConfig() Config {
	reserve := 6144
	if v := os.Getenv("TBX_BALLOON_RESERVE_MIB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			reserve = n
		}
	}
	return Config{ReserveMiB: reserve, FloorMiB: 1024, PollInterval: 5 * time.Second,
		DeadbandMiB: defaultDeadbandMiB, MinRetargetInterval: defaultMinRetargetInterval}
}

// Run polls host memory and reconciles balloons until stop is closed. vms is
// re-read each tick, so nodes appearing/leaving are picked up automatically.
func Run(cfg Config, vms func() map[string]Balloonable, stop <-chan struct{}) {
	RunWithLogger(cfg, vms, stop, nil)
}

// RunWithLogger is Run with an explicit telemetry sink. It emits a startup
// line so tbxd.log attests that the subsystem is running even while every
// node sits at its configured size, then one line per target change. On a
// platform whose host-memory probe is unsupported it emits a single inactivity
// line instead and returns.
func RunWithLogger(cfg Config, vms func() map[string]Balloonable, stop <-chan struct{}, logger Logger) {
	m := NewManager(logger)
	if cfg.DeadbandMiB > 0 {
		m.deadbandMiB = cfg.DeadbandMiB
	}
	if cfg.MinRetargetInterval > 0 {
		m.minRetargetInterval = cfg.MinRetargetInterval
	}
	probe := cfg.HostMemory
	if probe == nil && cfg.HostFreeMiB != nil {
		probe = func(context.Context) (hostmem.Snapshot, error) {
			free, err := cfg.HostFreeMiB()
			return hostmem.Snapshot{AvailableMiB: free}, err
		}
	}
	if probe == nil {
		probe = hostmem.SystemSnapshot
	}
	// The capability is checked once, before the loop: on a platform with no
	// host-memory probe there is nothing to poll, so the manager states it is
	// inactive and stands down rather than logging a failing read every poll
	// forever on the file `tbx logs` points operators at (#446).
	if _, err := probe(context.Background()); errors.Is(err, ErrUnsupported) {
		m.log("balloon: manager inactive: %v", err)
		return
	}
	m.log("balloon: manager started (reserve=%dMiB floor=%dMiB poll=%s)", cfg.ReserveMiB, cfg.FloorMiB, cfg.PollInterval)
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			sample, err := probe(context.Background())
			if err != nil {
				m.log("balloon: read host memory: %v", err)
				continue
			}
			if cfg.HoldMiB != nil {
				m.ReconcileSnapshot(vms(), sample, cfg.ReserveMiB, cfg.FloorMiB, cfg.HoldMiB(), now())
				continue
			}
			m.ReconcileSnapshot(vms(), sample, cfg.ReserveMiB, cfg.FloorMiB, 0, now())
		}
	}
}
