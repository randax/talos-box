package balloon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/hostmem"
)

// recordingVM captures SetMemoryTargetMiB calls.
type recordingVM struct {
	configured int
	target     int
	err        error
	calls      int
}

func (r *recordingVM) ConfiguredMiB() int { return r.configured }
func (r *recordingVM) SetMemoryTargetMiB(m int) error {
	r.calls++
	if r.err != nil {
		return r.err
	}
	r.target = m
	return nil
}

type currentTargetRecordingVM struct {
	recordingVM
	currentTarget int
}

func (r *currentTargetRecordingVM) CurrentTargetMiB() int { return r.currentTarget }
func (r *currentTargetRecordingVM) SetMemoryTargetMiB(targetMiB int) error {
	if err := r.recordingVM.SetMemoryTargetMiB(targetMiB); err != nil {
		return err
	}
	r.currentTarget = targetMiB
	return nil
}

func memorySample(availableMiB int) hostmem.Snapshot {
	return hostmem.Snapshot{TotalMiB: 32768, AvailableMiB: availableMiB, Pressure: hostmem.PressureNormal}
}

func TestManagerIgnoresDeficitsBelowDeadband(t *testing.T) {
	for _, deficit := range []int{0, 2, 88, 255} {
		t.Run(fmt.Sprintf("%dMiB", deficit), func(t *testing.T) {
			m := NewManager(nil)
			v := &recordingVM{configured: 4096, target: 4096}
			vms := map[string]Balloonable{"a": v}
			start := time.Unix(1000, 0)
			m.ReconcileSnapshot(vms, memorySample(6144-deficit), 6144, 1024, 0, start)
			if v.calls != 1 || v.target != 4096 {
				t.Fatalf("first tick with deficit %d applied %d target(s), target=%d; want one configured target", deficit, v.calls, v.target)
			}
			m.ReconcileSnapshot(vms, memorySample(6144-deficit), 6144, 1024, 0, start.Add(5*time.Second))
			if v.calls != 1 || v.target != 4096 {
				t.Fatalf("second tick with deficit %d applied %d target(s), target=%d; want no second apply", deficit, v.calls, v.target)
			}
		})
	}
}

func TestManagerRestoresExternallyInflatedNodeAfterHoldRelease(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 3072}

	m.ReconcileSnapshot(map[string]Balloonable{"a": v}, memorySample(8192), 6144, 1024, 0, time.Unix(1000, 0))

	if v.calls != 1 || v.target != 4096 {
		t.Fatalf("calls=%d target=%d, want one apply restoring the configured 4096 MiB target", v.calls, v.target)
	}
}

func TestManagerRestoresKnownNodeMovedOutsideManager(t *testing.T) {
	logs := &recordingLog{}
	m := NewManager(logs.Printf)
	v := &currentTargetRecordingVM{
		recordingVM:   recordingVM{configured: 4096, target: 4096},
		currentTarget: 4096,
	}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start)
	v.currentTarget = 2048
	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start.Add(5*time.Second))
	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start.Add(10*time.Second))

	if v.calls != 2 || v.target != 4096 || v.currentTarget != 4096 {
		t.Fatalf("calls=%d target=%d current=%d, want one correction restoring 4096 MiB and subsequent silence", v.calls, v.target, v.currentTarget)
	}
	lines := logs.snapshot()
	if got := lines[len(lines)-1]; got != "balloon a: target=4096MiB (restoring externally moved target 2048)" {
		t.Fatalf("correction log = %q, want distinct external-move restoration telemetry", got)
	}
}

func TestManagerRateLimitsPendingTargetUntilDeviceActivates(t *testing.T) {
	logs := &recordingLog{}
	m := NewManager(logs.Printf)
	v := &currentTargetRecordingVM{
		recordingVM:   recordingVM{configured: 4096, target: 4096, err: ErrTargetPending},
		currentTarget: 4096,
	}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start)
	if v.calls != 1 {
		t.Fatalf("first tick calls=%d, want one target attempt", v.calls)
	}
	if got := m.lastRetarget["a"]; !got.Equal(start) {
		t.Fatalf("lastRetarget=%v, want %v after pending target", got, start)
	}
	if !m.retryPending["a"] {
		t.Fatal("retryPending=false, want pending target retained for retry")
	}
	if lines := logs.snapshot(); len(lines) != 1 || !strings.Contains(lines[0], ErrTargetPending.Error()) {
		t.Fatalf("pending lines=%v, want exactly one pending line", lines)
	}

	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start.Add(5*time.Second))
	if v.calls != 1 {
		t.Fatalf("inside rate window calls=%d, want no retry", v.calls)
	}
	if lines := logs.snapshot(); len(lines) != 1 {
		t.Fatalf("inside rate window lines=%v, want pending streak logged once", lines)
	}

	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start.Add(time.Minute))
	if v.calls != 2 {
		t.Fatalf("at rate window calls=%d, want one retry", v.calls)
	}
	if lines := logs.snapshot(); len(lines) != 1 {
		t.Fatalf("retry lines=%v, want pending streak logged once", lines)
	}

	v.err = nil
	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start.Add(2*time.Minute))
	if v.calls != 3 || v.target != 4096 || m.last["a"] != 4096 {
		t.Fatalf("recovery calls=%d target=%d last=%d, want successful 4096 target recorded", v.calls, v.target, m.last["a"])
	}
	if m.pendingLogged["a"] {
		t.Fatal("pending log streak was not cleared after successful apply")
	}
	for _, line := range logs.snapshot() {
		if strings.Contains(line, "restoring externally moved target") {
			t.Fatalf("pending recovery emitted misleading external-move line: %q", line)
		}
	}
}

func TestManagerRestoresKnownNodeToLiveHoldTarget(t *testing.T) {
	m := NewManager(nil)
	v := &currentTargetRecordingVM{
		recordingVM:   recordingVM{configured: 4096, target: 4096},
		currentTarget: 4096,
	}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start)
	v.currentTarget = 3072
	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 1024, start.Add(5*time.Second))
	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 1024, start.Add(10*time.Second))

	if v.calls != 2 || v.target != 3072 || v.currentTarget != 3072 {
		t.Fatalf("calls=%d target=%d current=%d, want one correction applying the live hold-derived 3072 MiB target", v.calls, v.target, v.currentTarget)
	}
}

func TestManagerActsAtDeadbandBoundary(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	m.ReconcileSnapshot(map[string]Balloonable{"a": v}, memorySample(6144-256), 6144, 1024, 0, time.Unix(1000, 0))
	if v.calls != 1 || v.target != 4096-256 {
		t.Fatalf("calls=%d target=%d, want one target at 3840", v.calls, v.target)
	}
}

func TestManagerRetargetsAtMostOncePerMinute(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)
	m.ReconcileSnapshot(vms, memorySample(6144-256), 6144, 1024, 0, start)
	m.ReconcileSnapshot(vms, memorySample(6144-512), 6144, 1024, 0, start.Add(time.Minute-time.Nanosecond))
	if v.calls != 1 || v.target != 3840 {
		t.Fatalf("before minute calls=%d target=%d", v.calls, v.target)
	}
	m.ReconcileSnapshot(vms, memorySample(6144-512), 6144, 1024, 0, start.Add(time.Minute))
	if v.calls != 2 || v.target != 3584 {
		t.Fatalf("at minute calls=%d target=%d", v.calls, v.target)
	}
}

func TestManagerHighSwapHoldsExistingReclaim(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)
	m.ReconcileSnapshot(vms, memorySample(6144-512), 6144, 1024, 0, start)
	high := memorySample(8192)
	high.SwapTotalBytes, high.SwapAvailableBytes = 3<<30, 512<<20
	m.ReconcileSnapshot(vms, high, 6144, 1024, 0, start.Add(time.Minute))
	if v.calls != 1 || v.target != 3584 {
		t.Fatalf("high swap released reclaim: calls=%d target=%d", v.calls, v.target)
	}
}

func TestManagerCompressorPressureHoldsExistingReclaim(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)
	m.ReconcileSnapshot(vms, memorySample(6144-512), 6144, 1024, 0, start)
	high := memorySample(8192)
	high.CompressorMiB = 7000
	m.ReconcileSnapshot(vms, high, 6144, 1024, 0, start.Add(time.Minute))
	if v.calls != 1 || v.target != 3584 {
		t.Fatalf("compressor pressure released reclaim: calls=%d target=%d", v.calls, v.target)
	}
}

func TestManagerPressureLatchUsesLowWaterMarks(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)
	high := memorySample(6144 - 512)
	high.SwapTotalBytes, high.SwapAvailableBytes = 10<<30, 2<<30
	m.ReconcileSnapshot(vms, high, 6144, 1024, 0, start)
	between := memorySample(8192)
	between.SwapTotalBytes, between.SwapAvailableBytes = 10<<30, 3<<30
	m.ReconcileSnapshot(vms, between, 6144, 1024, 0, start.Add(time.Minute))
	if v.calls != 1 {
		t.Fatalf("70%% swap cleared latch: calls=%d", v.calls)
	}
	low := memorySample(8192)
	low.SwapTotalBytes, low.SwapAvailableBytes = 10<<30, 4<<30
	m.ReconcileSnapshot(vms, low, 6144, 1024, 0, start.Add(2*time.Minute))
	if v.calls != 2 || v.target != 4096 {
		t.Fatalf("low water did not release: calls=%d target=%d", v.calls, v.target)
	}
}

func TestManagerPressureSignalDoesNotCreateReclaimByItself(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	high := memorySample(8192)
	high.Pressure = hostmem.PressureCritical
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)
	m.ReconcileSnapshot(vms, high, 6144, 1024, 0, start)
	m.ReconcileSnapshot(vms, high, 6144, 1024, 0, start.Add(time.Minute))
	if v.calls != 1 || v.target != 4096 {
		t.Fatalf("pressure alone reclaimed memory: calls=%d target=%d", v.calls, v.target)
	}
}

func TestManagerPreBalloonHoldBypassesDeadbandAndRateLimit(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)
	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 1, start)
	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 2, start.Add(time.Second))
	if v.calls != 2 || v.target != 4094 {
		t.Fatalf("hold calls=%d target=%d, want immediate 4094", v.calls, v.target)
	}
}

func TestManagerHoldDoesNotBypassNaturalDeficitDeadband(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)

	for i, deficit := range []int{88, 2, 15} {
		m.ReconcileSnapshot(vms, memorySample(6144-deficit), 6144, 1024, 1, start.Add(time.Duration(i)*time.Second))
	}

	if v.calls != 1 || v.target != 4095 {
		t.Fatalf("calls=%d target=%d, want only the 1 MiB hold applied", v.calls, v.target)
	}
}

func TestManagerReleasesHoldAfterItClearsDespiteSmallDeficitDelta(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 1, start)
	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start.Add(2*time.Minute))

	if v.calls != 2 || v.target != 4096 {
		t.Fatalf("calls=%d target=%d, want the hold reclaimed and then released", v.calls, v.target)
	}
}

func TestManagerSkipsSmallDeficitWobblesAroundLastAppliedDeficit(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(6144-1024), 6144, 1024, 0, start)
	m.ReconcileSnapshot(vms, memorySample(6144-1050), 6144, 1024, 0, start.Add(time.Minute))
	m.ReconcileSnapshot(vms, memorySample(6144-950), 6144, 1024, 0, start.Add(2*time.Minute))

	if v.calls != 1 || v.target != 3072 {
		t.Fatalf("calls=%d target=%d, want the original 1024 MiB reclaim held", v.calls, v.target)
	}

	m.ReconcileSnapshot(vms, memorySample(6144-1300), 6144, 1024, 0, start.Add(3*time.Minute))
	if v.calls != 2 || v.target != 2796 {
		t.Fatalf("calls=%d target=%d, want a larger wobble to retarget", v.calls, v.target)
	}
}

func TestManagerPressureLatchClampsReleaseUntilClear(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(6144-1024), 6144, 1024, 0, start)
	latched := memorySample(6144 - 300)
	latched.SwapTotalBytes, latched.SwapAvailableBytes = 10<<30, 1<<30
	m.ReconcileSnapshot(vms, latched, 6144, 1024, 0, start.Add(time.Minute))
	if v.calls != 1 || v.target != 3072 {
		t.Fatalf("latched calls=%d target=%d, want no release while pressure is latched", v.calls, v.target)
	}

	cleared := memorySample(6144 - 300)
	cleared.SwapTotalBytes, cleared.SwapAvailableBytes = 10<<30, 4<<30
	m.ReconcileSnapshot(vms, cleared, 6144, 1024, 0, start.Add(2*time.Minute))
	if v.calls != 2 || v.target != 3796 {
		t.Fatalf("cleared calls=%d target=%d, want release to the 300 MiB deficit target", v.calls, v.target)
	}
}

func TestManagerPressureLatchClampsPlanToAbandonedPreBalloonTarget(t *testing.T) {
	m := NewManager(nil)
	v := &currentTargetRecordingVM{
		recordingVM:   recordingVM{configured: 4096, target: 4096},
		currentTarget: 4096,
	}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start)
	v.currentTarget = 3072
	latched := memorySample(6144 - 596)
	latched.Pressure = hostmem.PressureCritical
	m.ReconcileSnapshot(vms, latched, 6144, 1024, 0, start.Add(time.Minute))

	if v.target != 3072 || v.currentTarget != 3072 {
		t.Fatalf("latched target=%d current=%d, want abandoned pre-balloon target 3072 held instead of plan 3500", v.target, v.currentTarget)
	}
}

func TestManagerWithoutPressureLatchRestoresAbandonedPreBalloonTargetToPlan(t *testing.T) {
	m := NewManager(nil)
	v := &currentTargetRecordingVM{
		recordingVM:   recordingVM{configured: 4096, target: 4096},
		currentTarget: 4096,
	}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, 0, start)
	v.currentTarget = 3072
	m.ReconcileSnapshot(vms, memorySample(6144-596), 6144, 1024, 0, start.Add(time.Minute))

	if v.target != 3500 || v.currentTarget != 3500 {
		t.Fatalf("clear target=%d current=%d, want planned target 3500 restored", v.target, v.currentTarget)
	}
}

func TestManagerPartialSuccessRateLimitsOnlySuccessfulNode(t *testing.T) {
	m := NewManager(nil)
	success := &recordingVM{configured: 4096, target: 4096}
	retry := &recordingVM{configured: 4096, target: 4096, err: errors.New("apply failed")}
	vms := map[string]Balloonable{"a": success, "b": retry}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(6144-256), 6144, 1024, 0, start)
	retry.err = nil
	m.ReconcileSnapshot(vms, memorySample(6144-256), 6144, 1024, 0, start.Add(5*time.Second))

	if success.calls != 1 || success.target != 3968 {
		t.Fatalf("successful node calls=%d target=%d, want its first target held inside the rate window", success.calls, success.target)
	}
	if retry.calls != 2 || retry.target != 3968 {
		t.Fatalf("retry node calls=%d target=%d, want retry at the existing target", retry.calls, retry.target)
	}
}

func TestManagerPartialSuccessRetriesFailedNodeAtNewDeficitWithoutRetargetingSuccessfulPeer(t *testing.T) {
	m := NewManager(nil)
	success := &recordingVM{configured: 4096, target: 4096}
	retry := &recordingVM{configured: 4096, target: 4096, err: errors.New("apply failed")}
	vms := map[string]Balloonable{"a": success, "b": retry}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(6144-256), 6144, 1024, 0, start)
	retry.err = nil
	m.ReconcileSnapshot(vms, memorySample(6144-512), 6144, 1024, 0, start.Add(5*time.Second))

	if success.calls != 1 || success.target != 3968 {
		t.Fatalf("successful node calls=%d target=%d, want its first target held inside the rate window", success.calls, success.target)
	}
	if retry.calls != 2 || retry.target != 3840 {
		t.Fatalf("retry node calls=%d target=%d, want retry at the newer target", retry.calls, retry.target)
	}

	m.ReconcileSnapshot(vms, memorySample(6144-512), 6144, 1024, 0, start.Add(time.Minute))
	if success.calls != 2 || success.target != 3840 {
		t.Fatalf("successful node after rate window calls=%d target=%d, want deferred newer target", success.calls, success.target)
	}
	if retry.calls != 2 || retry.target != 3840 {
		t.Fatalf("retried node after peer window calls=%d target=%d, want no duplicate retarget", retry.calls, retry.target)
	}
}

func TestManagerRetriesKnownNodeAfterPartialFailure(t *testing.T) {
	m := NewManager(nil)
	success := &recordingVM{configured: 4096, target: 4096}
	retry := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"a": success, "b": retry}
	start := time.Unix(1000, 0)

	m.ReconcileSnapshot(vms, memorySample(6144-256), 6144, 1024, 0, start)
	retry.err = errors.New("apply failed")
	m.ReconcileSnapshot(vms, memorySample(6144-512), 6144, 1024, 0, start.Add(time.Minute))
	retry.err = nil
	m.ReconcileSnapshot(vms, memorySample(6144-512), 6144, 1024, 0, start.Add(time.Minute+5*time.Second))

	if success.calls != 2 || success.target != 3840 {
		t.Fatalf("successful node calls=%d target=%d, want its newer target held inside the rate window", success.calls, success.target)
	}
	if retry.calls != 3 || retry.target != 3840 {
		t.Fatalf("retry node calls=%d target=%d, want known node retried at the unchanged target", retry.calls, retry.target)
	}
}

func TestManagerFailedTargetDoesNotConsumeRateWindow(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096, err: errors.New("apply failed")}
	vms := map[string]Balloonable{"a": v}
	start := time.Unix(1000, 0)
	m.ReconcileSnapshot(vms, memorySample(6144-512), 6144, 1024, 0, start)
	v.err = nil
	m.ReconcileSnapshot(vms, memorySample(6144-512), 6144, 1024, 0, start.Add(time.Second))
	if v.calls != 2 || v.target != 3584 {
		t.Fatalf("recovery calls=%d target=%d", v.calls, v.target)
	}
}

func TestManagerIncidentSampleDoesNotFlapTargets(t *testing.T) {
	m := NewManager(nil)
	v := &recordingVM{configured: 4096, target: 4096}
	vms := map[string]Balloonable{"cluster/cp-1": v}
	start := time.Unix(1000, 0)
	for i, available := range []int{6056, 6142, 6129, 6167} {
		sample := memorySample(available)
		sample.CompressorMiB = 8419
		sample.SwapTotalBytes, sample.SwapAvailableBytes = 3<<30, 410<<20
		m.ReconcileSnapshot(vms, sample, 6144, 1024, 0, start.Add(time.Duration(i)*31*time.Second))
	}
	if v.calls != 1 || v.target != 4096 {
		t.Fatalf("incident samples flapped target: calls=%d target=%d", v.calls, v.target)
	}
}

func TestRunUsesSnapshotProbe(t *testing.T) {
	rec := &recordingLog{}
	stop := make(chan struct{})
	close(stop)
	RunWithLogger(Config{
		ReserveMiB: 6144, FloorMiB: 1024, PollInterval: time.Hour,
		HostMemory: func(context.Context) (hostmem.Snapshot, error) { return memorySample(8192), nil },
	}, func() map[string]Balloonable { return nil }, stop, rec.Printf)
	if got := rec.snapshot(); len(got) != 1 || !strings.Contains(got[0], "balloon: manager started") {
		t.Fatalf("startup lines = %v", got)
	}
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
	// The probe is stubbed because the manager capability-checks it before it
	// logs anything: this test is about the startup line on a platform that has
	// a host-memory probe, and only a darwin build has the real one.
	RunWithLogger(Config{ReserveMiB: 6144, FloorMiB: 1024, PollInterval: time.Hour,
		HostFreeMiB: func() (int, error) { return 8192, nil }},
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

	NewManager(nil).ReconcileSnapshot(vms, memorySample(freeBefore+reclaim), reserve, floor, reclaim, time.Unix(1000, 0))

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

// #446: on a platform with no host-memory probe the manager must not poll. It
// used to log a failing read every 5s forever, which drowned tbxd.log — the
// file `tbx logs` points operators at — on every non-macOS host.
func TestRunStandsDownWhenTheHostProbeIsUnsupported(t *testing.T) {
	rec := &recordingLog{}
	probes := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunWithLogger(Config{
			ReserveMiB:   6144,
			FloorMiB:     1024,
			PollInterval: time.Millisecond,
			HostFreeMiB:  func() (int, error) { probes++; return 0, ErrUnsupported },
		}, func() map[string]Balloonable { return nil }, make(chan struct{}), rec.Printf)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunWithLogger kept polling an unsupported host probe; want it to stand down")
	}
	got := rec.snapshot()
	if len(got) != 1 || !strings.Contains(got[0], "balloon: manager inactive") {
		t.Fatalf("lines = %v, want one manager-inactive line", got)
	}
	if !strings.Contains(got[0], ErrUnsupported.Error()) {
		t.Errorf("inactive line %q does not state why", got[0])
	}
	if probes != 1 {
		t.Errorf("probed the host %d times, want exactly the one capability check", probes)
	}
}

// A wrapped sentinel still counts as a missing capability: probes wrap their
// errors, and string matching is what this replaced.
func TestRunStandsDownOnAWrappedUnsupportedError(t *testing.T) {
	rec := &recordingLog{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunWithLogger(Config{PollInterval: time.Millisecond,
			HostFreeMiB: func() (int, error) { return 0, fmt.Errorf("host memory: %w", ErrUnsupported) },
		}, func() map[string]Balloonable { return nil }, make(chan struct{}), rec.Printf)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunWithLogger did not stand down on a wrapped ErrUnsupported")
	}
	if got := rec.snapshot(); len(got) != 1 || !strings.Contains(got[0], "manager inactive") {
		t.Fatalf("lines = %v, want one manager-inactive line", got)
	}
}

// A real probe failure on a platform that HAS a probe is transient: the manager
// starts, reports the failed read, and keeps polling.
func TestRunKeepsPollingAfterARealProbeFailure(t *testing.T) {
	rec := &recordingLog{}
	stop := make(chan struct{})
	reads := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunWithLogger(Config{ReserveMiB: 6144, FloorMiB: 1024, PollInterval: time.Millisecond,
			HostFreeMiB: func() (int, error) {
				select {
				case reads <- struct{}{}:
				default:
				}
				return 0, errors.New("vm_stat: signal: killed")
			},
		}, func() map[string]Balloonable { return nil }, stop, rec.Printf)
	}()
	// Two polls past the startup capability check prove the loop is running.
	for i := 0; i < 3; i++ {
		select {
		case <-reads:
		case <-time.After(2 * time.Second):
			t.Fatal("the manager stopped polling after a transient probe failure")
		}
	}
	close(stop)
	<-done
	got := rec.snapshot()
	if len(got) < 2 || !strings.Contains(got[0], "balloon: manager started") {
		t.Fatalf("lines = %v, want a manager-started line then read failures", got)
	}
	if !strings.Contains(got[1], "balloon: read host memory") {
		t.Errorf("second line = %q, want the read failure reported", got[1])
	}
}

// A hold taken from mixed results — one guest shrank, its sibling's balloon
// device was not up — is the successful guest's reduction only. The manager
// must not replan that hold over both guests and hand part of it back to the
// one that gave it, while the pending guest contributes nothing.
func TestManagerPlansHoldAroundPendingDevice(t *testing.T) {
	logs := &recordingLog{}
	m := NewManager(logs.Printf)
	applied := &currentTargetRecordingVM{
		recordingVM:   recordingVM{configured: 4096, target: 3583},
		currentTarget: 3583, // the provision gate already took 513 MiB
	}
	pending := &currentTargetRecordingVM{
		recordingVM:   recordingVM{configured: 4096, target: 4096, err: ErrTargetPending},
		currentTarget: 4096,
	}
	vms := map[string]Balloonable{"a": applied, "b": pending}
	start := time.Unix(1000, 0)
	const hold = 513

	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, hold, start)
	if applied.target != 3583 {
		t.Fatalf("applied guest target=%d after first tick, want 3583 (no release on the pending guest's behalf)", applied.target)
	}
	if !m.devicePending["b"] || pending.calls != 1 {
		t.Fatalf("pending guest devicePending=%t calls=%d, want planned around after one attempt", m.devicePending["b"], pending.calls)
	}

	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, hold, start.Add(5*time.Second))
	if applied.target != 3583 || pending.calls != 1 {
		t.Fatalf("inside window target=%d pendingCalls=%d, want steady", applied.target, pending.calls)
	}

	// The device comes up: the probe succeeds and the hold is re-shared.
	pending.err = nil
	m.ReconcileSnapshot(vms, memorySample(8192), 6144, 1024, hold, start.Add(time.Minute))
	if m.devicePending["b"] {
		t.Fatal("devicePending still set after the device answered")
	}
	if applied.target != 3840 || pending.target != 3840 {
		t.Fatalf("after activation targets a=%d b=%d, want the 513 MiB hold shared as 3840/3840", applied.target, pending.target)
	}
	if !slices.ContainsFunc(logs.snapshot(), func(l string) bool { return strings.Contains(l, "device active") }) {
		t.Fatalf("logs=%v, want the device activation attested", logs.snapshot())
	}
}
