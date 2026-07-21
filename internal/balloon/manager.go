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

// Reconcile computes and applies balloon targets for one poll: if host free
// memory is below the reserve, inflate proportionally to reclaim the deficit
// (never below floorMiB); otherwise deflate every node back to configured.
func Reconcile(vms map[string]Balloonable, hostFreeMiB, reserveMiB, floorMiB int) {
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
		if err := vms[name].SetMemoryTargetMiB(targets[name]); err != nil {
			log.Printf("balloon %s: %v", name, err)
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
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			free, err := HostFreeMiB()
			if err != nil {
				log.Printf("balloon: read host memory: %v", err)
				continue
			}
			Reconcile(vms(), free, cfg.ReserveMiB, cfg.FloorMiB)
		}
	}
}
