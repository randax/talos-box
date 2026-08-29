package daemon

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/randax/talos-box/internal/cluster"
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
	// A relaunched node has answered nothing yet, so its stall is aged from the
	// new start time, not from whatever the previous incarnation observed.
	s.reachability.forget(nodeKey(clusterName, nodeName))
	s.stalls.forget(nodeKey(clusterName, nodeName))
	s.reboots.forget(nodeKey(clusterName, nodeName))
}

// nodeKey is the cluster-scoped identity the daemon-side node trackers share.
func nodeKey(clusterName, nodeName string) string { return clusterName + "/" + nodeName }

// forgetNode drops every daemon-side observation of one node, so a removed node
// leaves nothing behind that a same-named successor could inherit.
func (s *Server) forgetNode(clusterName, nodeName string) {
	if nodes, ok := s.vmStarts[clusterName]; ok {
		delete(nodes, nodeName)
		if len(nodes) == 0 {
			delete(s.vmStarts, clusterName)
		}
	}
	s.reachability.forget(nodeKey(clusterName, nodeName))
	s.stalls.forget(nodeKey(clusterName, nodeName))
	s.reboots.forget(nodeKey(clusterName, nodeName))
}

// forgetCluster drops the observations of every node in a cluster.
func (s *Server) forgetCluster(clusterName string) {
	delete(s.vmStarts, clusterName)
	s.readiness.forget(clusterName)
	prefix := clusterName + "/"
	s.reachability.forgetPrefix(prefix)
	s.stalls.forgetPrefix(prefix)
	s.reboots.forgetPrefix(prefix)
}

// forgetAllNodeTracking clears the trackers wholesale — Shutdown closes every
// VM, so no observation of one survives the daemon.
func (s *Server) forgetAllNodeTracking() {
	s.vmStarts = nil
	s.reachability.forgetAll()
	s.stalls.forgetAll()
	s.readiness.forgetAll()
	s.reboots.forgetAll()
}

// nodeReachability is what the daemon has observed about one node since its VM
// was launched: whether it ever answered, and — if it answered and then went
// quiet — when the silence started.
type nodeReachability struct {
	answered bool
	since    time.Time
}

// reachabilityLog tracks the transition into PhaseUnreachable. It exists
// because VM uptime is not unreachability: a node that served for hours before
// going quiet must be aged from the moment it stopped answering (#288).
// refreshNodeStatuses runs outside opMu and concurrently per connection, so the
// log carries its own lock.
type reachabilityLog struct {
	mu    sync.Mutex
	nodes map[string]nodeReachability
}

// observe records one phase observation and reports when the node stopped
// answering, or nil when it has never answered since its VM launched — in that
// case the launch time is the only honest clock, and the caller uses it.
func (l *reachabilityLog) observe(key string, phase Phase, now time.Time) *time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nodes == nil {
		l.nodes = make(map[string]nodeReachability)
	}
	switch phase {
	case PhaseStopped, PhaseSuspended:
		// A stopped VM starts over: its next boot is aged from its own launch.
		delete(l.nodes, key)
		return nil
	case PhaseUnreachable:
		entry := l.nodes[key]
		if !entry.answered {
			return nil
		}
		if entry.since.IsZero() {
			entry.since = now
			l.nodes[key] = entry
		}
		since := entry.since
		return &since
	default:
		l.nodes[key] = nodeReachability{answered: true}
		return nil
	}
}

func (l *reachabilityLog) forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.nodes, key)
}

func (l *reachabilityLog) forgetPrefix(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	forgetPrefix(l.nodes, prefix)
}

func (l *reachabilityLog) forgetAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nodes = nil
}

// forgetPrefix deletes every cluster-scoped key belonging to one cluster.
func forgetPrefix[V any](entries map[string]V, prefix string) {
	for key := range entries {
		if strings.HasPrefix(key, prefix) {
			delete(entries, key)
		}
	}
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

func (l *stallLog) forgetPrefix(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	forgetPrefix(l.stalls, prefix)
}

func (l *stallLog) forgetAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stalls = nil
}

// defaultStallScanInterval is how often the daemon observes its own nodes. The
// stall threshold is minutes, so a 30s cadence records the crossing promptly
// while costing one apid probe per running node.
const defaultStallScanInterval = 30 * time.Second

// startStallWatch begins the daemon-side stall observation. logNodeStalls used
// to run only from dispatchStatus, so a node that stalled while nobody polled
// left nothing in tbxd.log at all (#288) — the log an operator is told to check.
// Serve starts the watch; Shutdown stops it.
func (s *Server) startStallWatch() {
	s.stallWatchMu.Lock()
	defer s.stallWatchMu.Unlock()
	if s.stallWatchStop != nil {
		return
	}
	interval := s.stallScanInterval
	if interval <= 0 {
		interval = defaultStallScanInterval
	}
	stop, done := make(chan struct{}), make(chan struct{})
	s.stallWatchStop, s.stallWatchDone = stop, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// one goroutine drives every scan, so a slow probe round
				// delays the next tick instead of overlapping with it
				s.observeNodeStalls()
			}
		}
	}()
}

// stopStallWatch stops the watch and waits for the in-flight scan to finish, so
// no observation can run against state Shutdown is tearing down.
func (s *Server) stopStallWatch() {
	s.stallWatchMu.Lock()
	stop, done := s.stallWatchStop, s.stallWatchDone
	s.stallWatchStop, s.stallWatchDone = nil, nil
	s.stallWatchMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// observeNodeStalls runs the same observation dispatchStatus runs, but holds
// opMu only for the in-memory VM inventory: the filesystem-backed cluster
// loads and the apid probes both run after the lock is released, so neither a
// slow disk nor a silent node can delay lifecycle operations. It skips the
// Kubernetes and storage probes, which no log line depends on, and clusters
// with no VMs at all — their nodes are stopped, and stop/destroy already
// forget their stall tracking.
//
// A failed cluster load is dropped silently: it repeats every interval, and a
// daemon that logged it would fill tbxd.log with the same line forever.
func (s *Server) observeNodeStalls() {
	type nodeSnapshot struct {
		running   bool
		startedAt *time.Time
	}
	s.opMu.Lock()
	inventory := make(map[string]map[string]nodeSnapshot, len(s.vms))
	for clusterName, nodes := range s.vms {
		snapshot := make(map[string]nodeSnapshot, len(nodes))
		for nodeName, machine := range nodes {
			snapshot[nodeName] = nodeSnapshot{
				running:   machine != nil && machine.Active(),
				startedAt: s.vmStartedAt(clusterName, nodeName),
			}
		}
		inventory[clusterName] = snapshot
	}
	s.opMu.Unlock()

	statuses := make([]ClusterStatus, 0, len(inventory))
	for clusterName, nodes := range inventory {
		item, err := cluster.Load(clusterName)
		if err != nil {
			continue
		}
		status := ClusterStatus{Name: item.Name, subnetIndex: item.SubnetIndex}
		for _, node := range item.Nodes {
			snapshot, tracked := nodes[node.Name]
			if !tracked {
				continue
			}
			status.Nodes = append(status.Nodes, NodeStatus{
				Name:      node.Name,
				Role:      node.Role,
				MAC:       node.MAC,
				Phase:     ClassifyPhase(snapshot.running, ProbeResult{}),
				StartedAt: snapshot.startedAt,
			})
		}
		if len(status.Nodes) > 0 {
			statuses = append(statuses, status)
		}
	}
	s.refreshNodeStatuses(statuses)
}

// logNodeStalls records a node crossing the stall threshold, and its later
// recovery, in the daemon log — the surface an operator is told to check when
// the CLI hint escalates.
func (s *Server) logNodeStalls(statuses []ClusterStatus, now time.Time) {
	for _, status := range statuses {
		for _, node := range status.Nodes {
			if node.Phase.Stopped() {
				// A stopped node is not a stalled node; forget it silently
				// so a later boot reports its own stall from scratch.
				s.stalls.forget(nodeKey(status.Name, node.Name))
				continue
			}
			age := node.UnreachableFor(now)
			stalled := node.Phase == PhaseUnreachable && age > nodeStallThreshold
			key := nodeKey(status.Name, node.Name)
			crossed, recoveredAfter, wasStalled := s.stalls.observe(key, stalled, age)
			switch {
			case crossed && node.answeredSinceStart():
				log.Printf("status %s: node %s stopped answering on apid %s ago; inspect it live: tbx console %s %s",
					status.Name, node.Name, formatStallDuration(age), status.Name, node.Name)
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
