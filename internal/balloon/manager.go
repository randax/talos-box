package balloon

import (
	"log"
	"os"
	"sort"
	"strconv"
	"time"
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
	log  Logger
	last map[string]int
}

// NewManager returns a manager logging through logger; a nil logger uses the
// standard logger, which in tbxd is tbxd.log.
func NewManager(logger Logger) *Manager {
	if logger == nil {
		logger = log.Printf
	}
	return &Manager{log: logger, last: map[string]int{}}
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
	ReserveMiB   int
	FloorMiB     int
	PollInterval time.Duration
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
	return Config{ReserveMiB: reserve, FloorMiB: 1024, PollInterval: 5 * time.Second}
}

// Run polls host memory and reconciles balloons until stop is closed. vms is
// re-read each tick, so nodes appearing/leaving are picked up automatically.
func Run(cfg Config, vms func() map[string]Balloonable, stop <-chan struct{}) {
	RunWithLogger(cfg, vms, stop, nil)
}

// RunWithLogger is Run with an explicit telemetry sink. It emits a startup
// line so tbxd.log attests that the subsystem is running even while every
// node sits at its configured size, then one line per target change.
func RunWithLogger(cfg Config, vms func() map[string]Balloonable, stop <-chan struct{}, logger Logger) {
	m := NewManager(logger)
	m.log("balloon: manager started (reserve=%dMiB floor=%dMiB poll=%s)", cfg.ReserveMiB, cfg.FloorMiB, cfg.PollInterval)
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			free, err := HostFreeMiB()
			if err != nil {
				m.log("balloon: read host memory: %v", err)
				continue
			}
			m.Reconcile(vms(), free, cfg.ReserveMiB, cfg.FloorMiB)
		}
	}
}
