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

// Balloonable is the balloon manager's view of a running configured VM.
type Balloonable interface {
	ConfiguredMiB() int
	SetMemoryTargetMiB(int) error
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
	lastRetarget        time.Time
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
	return &Manager{log: logger, last: map[string]int{}, deadbandMiB: defaultDeadbandMiB, minRetargetInterval: defaultMinRetargetInterval}
}

// ReconcileSnapshot applies the steady-state policy to one shared memory
// sample. Pressure signals veto release of existing reclaim; they never create
// reclaim without a real available-memory deficit.
func (m *Manager) ReconcileSnapshot(vms map[string]Balloonable, sample hostmem.Snapshot, reserveMiB, floorMiB, holdMiB int, now time.Time) {
	m.updatePressureLatch(sample)
	deficit := reserveMiB - sample.AvailableMiB
	if deficit < 0 {
		deficit = 0
	}
	bypass := holdMiB > 0
	if holdMiB > deficit {
		deficit = holdMiB
	}

	if !bypass && deficit > 0 && deficit < m.deadbandMiB {
		m.dropDeparted(vms)
		return
	}
	if !bypass && deficit == 0 && m.pressureLatched {
		m.dropDeparted(vms)
		return
	}

	nodes := make([]Node, 0, len(vms))
	for name, vm := range vms {
		nodes = append(nodes, Node{Name: name, ConfiguredMiB: vm.ConfiguredMiB()})
	}
	targets := PlanTargets(nodes, deficit, floorMiB)
	changed := false
	for name, target := range targets {
		previous, known := m.last[name]
		if (!known && target != vms[name].ConfiguredMiB()) || (known && previous != target) {
			changed = true
			break
		}
	}
	if !changed {
		m.dropDeparted(vms)
		return
	}
	if !bypass && !m.lastRetarget.IsZero() && now.Sub(m.lastRetarget) < m.minRetargetInterval {
		m.dropDeparted(vms)
		return
	}

	names := make([]string, 0, len(vms))
	for name := range vms {
		names = append(names, name)
	}
	sort.Strings(names)
	allSucceeded := true
	for _, name := range names {
		target := targets[name]
		previous, known := m.last[name]
		if (!known && target == vms[name].ConfiguredMiB()) || (known && previous == target) {
			continue
		}
		if err := vms[name].SetMemoryTargetMiB(target); err != nil {
			m.log("balloon %s: %v", name, err)
			delete(m.last, name)
			allSucceeded = false
			continue
		}
		m.last[name] = target
		m.log("balloon %s: target=%dMiB (configured=%d hostFree=%d reserve=%d deficit=%d compressor=%dMiB swapUsed=%d%% pressureLatched=%t)",
			name, target, vms[name].ConfiguredMiB(), sample.AvailableMiB, reserveMiB, deficit,
			sample.CompressorMiB, snapshotSwapPercent(sample), m.pressureLatched)
	}
	if allSucceeded {
		m.lastRetarget = now
	}
	m.dropDeparted(vms)
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
		}
	}
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

// holdAdjustedFreeMiB is the host-free reading Reconcile must act on while a
// pre-balloon is held: low enough that the reconcile's deficit
// (reserve - free) is at least holdMiB, so the held reclaim stays out of the
// guests until the hold expires. Debiting the hold from free instead cannot do
// that: the reclaim is already in the measured reading, so free - hold is
// algebraically the pre-reclaim reading, which on a host that was above the
// reserve to begin with yields a deficit of zero and deflates every guest back
// to configured while the admitted guest is still booting. Real pressure below
// the cap still wins — the reading is only capped, never raised.
func holdAdjustedFreeMiB(freeMiB, reserveMiB, holdMiB int) int {
	if holdMiB <= 0 {
		return freeMiB
	}
	if capped := reserveMiB - holdMiB; freeMiB > capped {
		return capped
	}
	return freeMiB
}
