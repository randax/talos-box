// Package balloon computes virtio memory-balloon targets and the overcommit
// warning. tbxd's manager reclaims guest RAM under host memory pressure by
// setting each running configured node's balloon target below its configured
// size — never below a per-node floor (SPEC §8).
package balloon

import "sort"

// Node is one running, configured (non-maintenance) VM eligible for ballooning.
type Node struct {
	Name          string
	ConfiguredMiB int
}

// PlanTargets returns each node's balloon target (MiB) needed to reclaim
// deficitMiB of host memory, distributed in proportion to node size but never
// pushing any node below floorMiB. When some nodes hit the floor, their unmet
// share is redistributed across the nodes still above it (water-filling).
func PlanTargets(nodes []Node, deficitMiB, floorMiB int) map[string]int {
	targets := make(map[string]int, len(nodes))
	for _, n := range nodes {
		targets[n.Name] = n.ConfiguredMiB
	}
	if deficitMiB <= 0 {
		return targets
	}

	// Distribute the deficit proportionally to configured size. Any node that
	// would fall below the floor is clamped there and its unmet share is
	// redistributed across the rest — repeat until the split is stable.
	active := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		if n.ConfiguredMiB > floorMiB {
			active[n.Name] = n
		}
	}
	remaining := deficitMiB
	for remaining > 0 && len(active) > 0 {
		weight := 0
		for _, n := range active {
			weight += n.ConfiguredMiB
		}
		clamped := false
		// process in stable name order so the arithmetic is deterministic
		names := make([]string, 0, len(active))
		for name := range active {
			names = append(names, name)
		}
		sort.Strings(names)
		distributed := 0
		for _, name := range names {
			n := active[name]
			share := remaining * n.ConfiguredMiB / weight
			target := targets[name] - share
			if target <= floorMiB {
				// clamp to floor and drop from the active pool
				distributed += targets[name] - floorMiB
				targets[name] = floorMiB
				delete(active, name)
				clamped = true
			} else {
				targets[name] = target
				distributed += share
			}
		}
		remaining -= distributed
		if !clamped {
			// everyone took their proportional share; any sub-MiB residual
			// from integer division is intentionally dropped (never over-reclaims)
			break
		}
	}
	return targets
}

// Overcommit reports whether the planned total VM memory exceeds the host RAM
// minus the reserve — the condition that triggers the create/start warning.
func Overcommit(plannedMiB, hostRAMMiB, reserveMiB int) bool {
	return plannedMiB > hostRAMMiB-reserveMiB
}
