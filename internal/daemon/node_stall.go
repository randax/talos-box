package daemon

import (
	"log"
	"sync"
	"time"
)

// Node stall observability (#288): a node that never answers apid used to be
// invisible — the status hint repeated its calm boot message forever and the
// daemon log said nothing. The daemon knows when it launched each VM, so it can
// both age the hint and log the transition into and out of a stall.

// recordVMStart stamps a node's VM launch. Callers hold opMu (every launch runs
// under it), and a relaunch overwrites the previous stamp, so a stopped node's
// stale stamp can never make a fresh boot look old.
func (s *Server) recordVMStart(clusterName, nodeName string) {
	if s.vmStarts == nil {
		s.vmStarts = make(map[string]map[string]time.Time)
	}
	nodes, ok := s.vmStarts[clusterName]
	if !ok {
		nodes = make(map[string]time.Time)
		s.vmStarts[clusterName] = nodes
	}
	nodes[nodeName] = time.Now()
}

// vmStartedAt reports when this daemon launched a node's VM, if it did.
func (s *Server) vmStartedAt(clusterName, nodeName string) *time.Time {
	started, ok := s.vmStarts[clusterName][nodeName]
	if !ok {
		return nil
	}
	return &started
}

// stallLog remembers which nodes have already been reported as stalled, so the
// daemon log records the two moments that matter — the crossing and the
// recovery — instead of one line per status poll.
type stallLog struct {
	mu     sync.Mutex
	stalls map[string]time.Duration
}

func (l *stallLog) observe(key string, stalled bool, age time.Duration) (crossed bool, recovered time.Duration, wasStalled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stalls == nil {
		l.stalls = make(map[string]time.Duration)
	}
	previous, known := l.stalls[key]
	switch {
	case stalled && !known:
		l.stalls[key] = age
		return true, 0, false
	case stalled:
		l.stalls[key] = age
		return false, 0, true
	case known:
		delete(l.stalls, key)
		return false, previous, true
	default:
		return false, 0, false
	}
}

func (l *stallLog) forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.stalls, key)
}

// logNodeStalls records a node crossing the stall threshold, and its later
// recovery, in the daemon log — the surface an operator is told to check when
// the CLI hint escalates.
func (s *Server) logNodeStalls(statuses []ClusterStatus, now time.Time) {
	for _, status := range statuses {
		for _, node := range status.Nodes {
			if node.Phase == PhaseStopped {
				// A stopped node is not a stalled node; forget it silently
				// so a later boot reports its own stall from scratch.
				s.stalls.forget(status.Name + "/" + node.Name)
				continue
			}
			age := node.UnreachableFor(now)
			stalled := node.Phase == PhaseUnreachable && age > nodeStallThreshold
			key := status.Name + "/" + node.Name
			crossed, recoveredAfter, wasStalled := s.stalls.observe(key, stalled, age)
			switch {
			case crossed:
				log.Printf("status %s: node %s has not answered on apid for %s since its VM started; inspect it live: tbx console %s %s",
					status.Name, node.Name, formatStallDuration(age), status.Name, node.Name)
			case !stalled && wasStalled:
				log.Printf("status %s: node %s answered again after %s unreachable (phase %s)",
					status.Name, node.Name, formatStallDuration(recoveredAfter), node.Phase)
			}
		}
	}
}
