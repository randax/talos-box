package daemon

import (
	"sync/atomic"
	"testing"
)

// The holder registers first and the waiter arrives later: the interrupt has to
// fire when the request comes in.
func TestPreemptionRequestFiresRegisteredInterrupts(t *testing.T) {
	var preemptions preemptions
	var interrupted atomic.Int32
	remove := preemptions.register("demo", func() { interrupted.Add(1) })
	defer remove()

	if got := interrupted.Load(); got != 0 {
		t.Fatalf("interrupts fired %d times with nobody waiting, want 0", got)
	}
	release := preemptions.request("Demo") // the mutation lock is keyed case-insensitively too
	if got := interrupted.Load(); got != 1 {
		t.Fatalf("interrupts fired %d times for a queued operation, want 1", got)
	}

	// Once the waiter holds the lock it is the holder, and work registered
	// afterwards is its own: it must not interrupt itself.
	release()
	var later atomic.Int32
	removeLater := preemptions.register("demo", func() { later.Add(1) })
	defer removeLater()
	if got := later.Load(); got != 0 {
		t.Fatalf("interrupt fired %d times after the waiter took the lock, want 0", got)
	}
}

// The other order: the waiter announces itself before the holder has anything
// to offer. Registering must fire at once, or the waiter queues for the whole
// budget it just asked to skip.
func TestPreemptionRegisterFiresWhenAnOperationIsAlreadyQueued(t *testing.T) {
	var preemptions preemptions
	release := preemptions.request("demo")
	defer release()

	var interrupted atomic.Int32
	remove := preemptions.register("demo", func() { interrupted.Add(1) })
	defer remove()
	if got := interrupted.Load(); got != 1 {
		t.Fatalf("interrupt fired %d times with an operation already queued, want 1", got)
	}

	// A removed interrupt belongs to a finished holder and must stay silent,
	// and another cluster's waiter is none of this one's business.
	remove()
	preemptions.request("demo")()
	preemptions.request("other")()
	if got := interrupted.Load(); got != 1 {
		t.Fatalf("interrupt fired %d times after removal, want it to stay at 1", got)
	}
}
