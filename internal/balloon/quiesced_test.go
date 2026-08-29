package balloon

import (
	"testing"
	"time"

	"github.com/randax/talos-box/internal/hostmem"
)

type quiescedVM struct {
	configured int
	quiesced   bool
	targets    []int
}

func (v *quiescedVM) ConfiguredMiB() int { return v.configured }
func (v *quiescedVM) SetMemoryTargetMiB(t int) error {
	if v.quiesced {
		return ErrQuiesced
	}
	v.targets = append(v.targets, t)
	return nil
}

// A retarget refused with ErrQuiesced is "not this tick": no log, no ledger
// movement, so the last applied target still clamps once the latch lifts (#513).
func TestQuiescedRetargetLeavesLedgerAlone(t *testing.T) {
	var logged []string
	m := NewManager(func(format string, args ...any) { logged = append(logged, format) })
	vm := &quiescedVM{configured: 4096}
	vms := map[string]Balloonable{"c/n": vm}
	now := time.Now()
	// deficit 2048 → target 2048 applied and remembered
	m.ReconcileSnapshot(vms, hostmem.Snapshot{AvailableMiB: 4096}, 6144, 1024, 0, now)
	if got := m.last["c/n"]; got != 2048 {
		t.Fatalf("first tick target = %d, want 2048 (targets %v)", got, vm.targets)
	}
	vm.quiesced = true
	before := len(logged)
	// deficit gone → would restore to 4096, but the teardown refuses
	m.ReconcileSnapshot(vms, hostmem.Snapshot{AvailableMiB: 16384}, 6144, 1024, 0, now.Add(2*time.Minute))
	if got := m.last["c/n"]; got != 2048 {
		t.Fatalf("ledger moved on ErrQuiesced: last = %d, want 2048", got)
	}
	if m.retryPending["c/n"] || len(logged) != before {
		t.Fatalf("ErrQuiesced marked retry (%t) or logged (%d lines)", m.retryPending["c/n"], len(logged)-before)
	}
}
