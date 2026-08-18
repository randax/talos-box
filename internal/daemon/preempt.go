package daemon

import (
	"strings"
	"sync"
)

// preemptions is the handover channel between the operation holding a cluster's
// mutation lock and the ones queued behind it. A create owns its cluster for
// minutes — the boot wait, then the reconcile — and a destroy queued behind it
// wants none of that finished: the nodes it is waiting on are about to be
// deleted. The queued operation announces itself here before it blocks on the
// lock, and the holder registers the work it is willing to abandon.
//
// Locking: this type never holds its own mutex while running an interrupt, and
// callers never hold the cluster mutation lock or opMu while announcing. It is
// therefore a leaf in the daemon's lock order (cluster mutation lock, then
// opMu), and an interrupt is free to take opMu itself.
//
// The zero value is ready to use.
type preemptions struct {
	mu       sync.Mutex
	sequence uint64
	// pending counts the operations queued on each cluster's mutation lock.
	pending map[string]int
	// interrupts holds what the current holder offered to abandon, keyed by
	// cluster and then by registration so one holder's removal cannot drop
	// another's.
	interrupts map[string]map[uint64]func()
}

// request announces an operation about to queue on the cluster's mutation lock.
// It fires every interrupt registered for that cluster and returns the release
// the caller runs once it holds the lock — from that point it is the holder,
// not a waiter, and a later holder must not be interrupted on its behalf.
func (p *preemptions) request(name string) (release func()) {
	key := clusterKey(name)
	p.mu.Lock()
	if p.pending == nil {
		p.pending = make(map[string]int)
	}
	p.pending[key]++
	interrupts := make([]func(), 0, len(p.interrupts[key]))
	for _, interrupt := range p.interrupts[key] {
		interrupts = append(interrupts, interrupt)
	}
	p.mu.Unlock()
	for _, interrupt := range interrupts {
		interrupt()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.pending[key]--; p.pending[key] <= 0 {
				delete(p.pending, key)
			}
		})
	}
}

// register records an interrupt for the cluster and returns its removal. It
// fires the interrupt immediately when an operation is already queued: a
// request that arrived before the holder registered still has to reach it,
// otherwise the waiter queues for the full budget it just asked to skip.
func (p *preemptions) register(name string, interrupt func()) (remove func()) {
	key := clusterKey(name)
	p.mu.Lock()
	if p.interrupts == nil {
		p.interrupts = make(map[string]map[uint64]func())
	}
	if p.interrupts[key] == nil {
		p.interrupts[key] = make(map[uint64]func())
	}
	p.sequence++
	id := p.sequence
	p.interrupts[key][id] = interrupt
	queued := p.pending[key] > 0
	p.mu.Unlock()
	if queued {
		interrupt()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			delete(p.interrupts[key], id)
			if len(p.interrupts[key]) == 0 {
				delete(p.interrupts, key)
			}
		})
	}
}

// clusterKey normalizes a cluster name for the daemon's per-cluster maps: on a
// case-insensitive filesystem "Demo" and "demo" load the same cluster state and
// must share one mutation lock and one preemption entry.
func clusterKey(name string) string {
	return strings.ToLower(name)
}
