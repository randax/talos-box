package daemon

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
)

// safeStages records narration a goroutine emits while the test reads it.
type safeStages struct {
	mu    sync.Mutex
	lines []string
}

func (s *safeStages) sink() stageFunc {
	return func(line string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lines = append(s.lines, line)
	}
}

func (s *safeStages) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// The client's liveness bound measures silence and is re-armed by every stage,
// so the wait for the operation lock has to narrate: without it a healthy verb
// queued behind a long, quiet operation fails with "tbxd stopped reporting
// progress" (#392).
func TestLockOperationNarratesAndRepeatsTheQueueWait(t *testing.T) {
	previous := opWaitNarrationInterval
	opWaitNarrationInterval = time.Millisecond
	t.Cleanup(func() { opWaitNarrationInterval = previous })

	service := &Server{}
	service.opMu.Lock()
	recorder := &safeStages{}
	held := make(chan struct{})
	go func() {
		service.lockOperation(recorder.sink())
		close(held)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		lines := recorder.snapshot()
		if len(lines) >= 2 {
			if !strings.Contains(lines[0], "waiting for the daemon's current operation") {
				t.Fatalf("first stage = %q, want the queue wait named", lines[0])
			}
			if !strings.Contains(lines[1], "still waiting") {
				t.Fatalf("second stage = %q, want the wait repeated", lines[1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue wait narration = %q, want it repeated while blocked", lines)
		}
		time.Sleep(time.Millisecond)
	}

	service.opMu.Unlock()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("lockOperation did not take the lock once it was free")
	}
	service.opMu.Unlock()
}

// An uncontended lock is the common case and must stay quiet: narration that
// fires every time would bury the stages that say what the verb is doing.
func TestLockOperationIsSilentWhenTheLockIsFree(t *testing.T) {
	service := &Server{}
	recorder := &safeStages{}
	service.lockOperation(recorder.sink())
	service.opMu.Unlock()
	if lines := recorder.snapshot(); len(lines) != 0 {
		t.Fatalf("narration = %q, want none for an uncontended lock", lines)
	}
}

// `up` creates its clusters under the operation lock, and a cold image fetch is
// minutes of work: the pass has to narrate them, or the client's liveness bound
// has nothing to re-arm it. Each line names the cluster it belongs to (#392).
func TestUpNarratesTheCreatesItRuns(t *testing.T) {
	service := newBootWaitCreateServer(t)
	restoreInterval(t, time.Millisecond, 30*time.Second)
	service.nodeProbe = func(string) ProbeResult {
		return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}
	}
	raw, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{{
		Name:          "booted",
		ControlPlanes: 1,
		Node:          cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
		Talos:         config.TalosSpec{Schematic: "aaa"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	progress, stages := recordStages()

	response := service.dispatchProvisioning(Request{Op: "up", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("up failed: %s", response.Error)
	}
	if indexOfStage(*stages, "booted: starting 1 node(s)") < 0 {
		t.Fatalf("up narration = %q, want the create stages named per cluster", *stages)
	}
}
