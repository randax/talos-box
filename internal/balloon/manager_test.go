package balloon

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingVM captures SetMemoryTargetMiB calls.
type recordingVM struct {
	configured int
	target     int
	err        error
}

func (r *recordingVM) ConfiguredMiB() int { return r.configured }
func (r *recordingVM) SetMemoryTargetMiB(m int) error {
	if r.err != nil {
		return r.err
	}
	r.target = m
	return nil
}

// recordingLog collects the manager's telemetry lines.
type recordingLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLog) Printf(format string, v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, v...))
}

func (l *recordingLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func TestReconcileInflatesUnderPressure(t *testing.T) {
	vms := map[string]Balloonable{
		"a": &recordingVM{configured: 4096},
		"b": &recordingVM{configured: 4096},
	}
	// host free 4096, reserve 6144 -> deficit 2048, split across two equal nodes
	Reconcile(vms, 4096, 6144, 1024)
	for name, v := range vms {
		got := v.(*recordingVM).target
		if got != 3072 {
			t.Errorf("node %s target = %d, want 3072", name, got)
		}
	}
}

func TestReconcileDeflatesWhenPressureReleases(t *testing.T) {
	v := &recordingVM{configured: 4096, target: 3000}
	vms := map[string]Balloonable{"a": v}
	// host free 8000 > reserve 6144 -> no deficit -> deflate to configured
	Reconcile(vms, 8000, 6144, 1024)
	if v.target != 4096 {
		t.Errorf("target = %d, want configured 4096 (deflated)", v.target)
	}
}

func TestReconcileRespectsFloor(t *testing.T) {
	v := &recordingVM{configured: 2048}
	vms := map[string]Balloonable{"a": v}
	// huge deficit but floor 1024 caps inflation
	Reconcile(vms, 0, 100000, 1024)
	if v.target != 1024 {
		t.Errorf("target = %d, want floor 1024", v.target)
	}
}

func TestManagerLogsOnTargetChangeAndStaysQuietWhenSteady(t *testing.T) {
	rec := &recordingLog{}
	m := NewManager(rec.Printf)
	v := &recordingVM{configured: 4096}
	vms := map[string]Balloonable{"a": v}

	m.Reconcile(vms, 4096, 6144, 1024) // deficit 2048 -> target 2048
	first := rec.snapshot()
	if len(first) != 1 {
		t.Fatalf("first reconcile logged %d lines, want 1: %v", len(first), first)
	}
	for _, want := range []string{"balloon a:", "target=2048MiB", "configured=4096", "hostFree=4096", "reserve=6144", "deficit=2048"} {
		if !strings.Contains(first[0], want) {
			t.Errorf("log line %q missing %q", first[0], want)
		}
	}

	// Steady state: identical readings must not log again.
	m.Reconcile(vms, 4096, 6144, 1024)
	m.Reconcile(vms, 4096, 6144, 1024)
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("steady state logged %d lines, want 1: %v", len(got), got)
	}

	// Pressure releases: target moves back to configured -> one more line.
	m.Reconcile(vms, 8000, 6144, 1024)
	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("deflate logged %d lines total, want 2: %v", len(got), got)
	}
	if !strings.Contains(got[1], "target=4096MiB") || !strings.Contains(got[1], "deficit=0") {
		t.Errorf("deflate line = %q, want target=4096MiB deficit=0", got[1])
	}
}

func TestManagerLogsErrorAndRelogsAfterRecovery(t *testing.T) {
	rec := &recordingLog{}
	m := NewManager(rec.Printf)
	v := &recordingVM{configured: 4096, err: errors.New("device not active")}
	vms := map[string]Balloonable{"a": v}

	m.Reconcile(vms, 4096, 6144, 1024)
	got := rec.snapshot()
	if len(got) != 1 || !strings.Contains(got[0], "device not active") {
		t.Fatalf("error reconcile lines = %v, want one error line", got)
	}

	// Once the node accepts targets again the same target must be re-logged,
	// so the log shows the recovery rather than staying silent.
	v.err = nil
	m.Reconcile(vms, 4096, 6144, 1024)
	got = rec.snapshot()
	if len(got) != 2 || !strings.Contains(got[1], "target=2048MiB") {
		t.Fatalf("recovery lines = %v, want a target line after the error", got)
	}
}

func TestManagerRelogsWhenNodeReturns(t *testing.T) {
	rec := &recordingLog{}
	m := NewManager(rec.Printf)
	v := &recordingVM{configured: 4096}
	vms := map[string]Balloonable{"a": v}

	m.Reconcile(vms, 4096, 6144, 1024)
	m.Reconcile(map[string]Balloonable{}, 4096, 6144, 1024) // node stops
	m.Reconcile(vms, 4096, 6144, 1024)                      // node comes back
	if got := rec.snapshot(); len(got) != 2 {
		t.Fatalf("lines = %v, want 2 (initial + re-appearance)", got)
	}
}

func TestRunLogsStartupLine(t *testing.T) {
	rec := &recordingLog{}
	stop := make(chan struct{})
	close(stop)
	RunWithLogger(Config{ReserveMiB: 6144, FloorMiB: 1024, PollInterval: time.Hour},
		func() map[string]Balloonable { return nil }, stop, rec.Printf)
	got := rec.snapshot()
	if len(got) != 1 || !strings.Contains(got[0], "balloon: manager started") {
		t.Fatalf("startup lines = %v, want one manager-started line", got)
	}
	for _, want := range []string{"reserve=6144MiB", "floor=1024MiB", "poll=1h0m0s"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("startup line %q missing %q", got[0], want)
		}
	}
}

// #398: the provision-start gate pre-balloons memory out of the running guests
// and holds it while the admitted guest boots. The hold has to survive the very
// next poll — the reclaim is already in the host-free reading, so debiting it
// reproduces the pre-reclaim number and deflates every guest back to configured
// seconds before the new guest claims anything.
func TestHoldKeepsThePreBalloonedTargetsInPlace(t *testing.T) {
	const reserve, floor, configured = 6144, 1024, 2048
	// The gate's own numbers: 7680 MiB free, a 2048 MiB guest starting, so the
	// shortfall against the reserve is 512 MiB and the reclaim puts host free
	// at 8192 MiB — above the reserve.
	const freeBefore, reclaim = 7680, 512
	vms := map[string]Balloonable{
		"a": &recordingVM{configured: configured, target: configured - reclaim/3},
		"b": &recordingVM{configured: configured, target: configured - reclaim/3},
		"c": &recordingVM{configured: configured, target: configured - reclaim/3},
	}

	Reconcile(vms, holdAdjustedFreeMiB(freeBefore+reclaim, reserve, reclaim), reserve, floor)

	held := 0
	for name, v := range vms {
		target := v.(*recordingVM).target
		if target >= configured {
			t.Fatalf("node %s back at %d MiB: the reconcile handed the pre-balloon straight back", name, target)
		}
		if target < floor {
			t.Fatalf("node %s target %d MiB is below the %d MiB floor", name, target, floor)
		}
		held += configured - target
	}
	// PlanTargets drops the sub-MiB residual of its proportional split, so the
	// reconcile may land up to one MiB per node short of the reclaim.
	if held < reclaim-len(vms) {
		t.Fatalf("reconcile held %d MiB out of the guests, want the %d MiB pre-balloon less the per-node rounding", held, reclaim)
	}
}

// The hold is a floor on the deficit, never a ceiling: a host under real
// pressure still reclaims what the pressure calls for.
func TestHoldNeverRaisesTheHostFreeReading(t *testing.T) {
	const reserve = 6144
	if got := holdAdjustedFreeMiB(4096, reserve, 512); got != 4096 {
		t.Errorf("holdAdjustedFreeMiB(4096, %d, 512) = %d, want the measured reading under real pressure", reserve, got)
	}
	if got := holdAdjustedFreeMiB(8192, reserve, 0); got != 8192 {
		t.Errorf("holdAdjustedFreeMiB(8192, %d, 0) = %d, want the reading untouched with nothing held", reserve, got)
	}
}
