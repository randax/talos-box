package daemon

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func unreachableNodeStatus(name string, started time.Time) ClusterStatus {
	return ClusterStatus{
		Name:  "qa-dom",
		Nodes: []NodeStatus{{Name: name, Phase: PhaseUnreachable, StartedAt: &started}},
	}
}

func TestHintsStayCalmInsideTheBootWindow(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	hints := hintsAt(unreachableNodeStatus("qa-dom-worker-1", now.Add(-30*time.Second)), now)
	joined := strings.Join(hints, "\n")
	if !strings.Contains(joined, "boot takes ~1 minute") {
		t.Fatalf("hints = %q, want the calm first-window hint", joined)
	}
	if strings.Contains(joined, "tbx console") {
		t.Fatalf("hints = %q, want no escalation inside the boot window", joined)
	}
}

func TestHintsEscalateWhenNodeStaysUnreachablePastTheBootWindow(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	hints := hintsAt(unreachableNodeStatus("qa-dom-worker-1", now.Add(-6*time.Minute)), now)
	joined := strings.Join(hints, "\n")
	for _, want := range []string{"qa-dom-worker-1", "6m", "tbx console qa-dom qa-dom-worker-1", "tbx doctor"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("hints = %q, want substring %q", joined, want)
		}
	}
	if strings.Contains(joined, "boot takes ~1 minute") {
		t.Fatalf("hints = %q, want the calm hint replaced by the escalation", joined)
	}
}

func TestHintsKeepCalmHintForFreshNodeAlongsideStalledNode(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	stalled := now.Add(-10 * time.Minute)
	fresh := now.Add(-20 * time.Second)
	status := ClusterStatus{
		Name: "qa-dom",
		Nodes: []NodeStatus{
			{Name: "qa-dom-worker-1", Phase: PhaseUnreachable, StartedAt: &stalled},
			{Name: "qa-dom-worker-2", Phase: PhaseUnreachable, StartedAt: &fresh},
		},
	}
	joined := strings.Join(hintsAt(status, now), "\n")
	if !strings.Contains(joined, "1 node(s) not answering yet") {
		t.Fatalf("hints = %q, want the calm hint for the freshly booted node", joined)
	}
	if !strings.Contains(joined, "tbx console qa-dom qa-dom-worker-1") {
		t.Fatalf("hints = %q, want the escalation for the stalled node", joined)
	}
	if strings.Contains(joined, "qa-dom-worker-2 ") {
		t.Fatalf("hints = %q, want the fresh node left out of the escalation", joined)
	}
}

func TestHintsKeepCalmHintWhenNodeStartTimeIsUnknown(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	status := ClusterStatus{Name: "qa-dom", Nodes: []NodeStatus{{Name: "qa-dom-worker-1", Phase: PhaseUnreachable}}}
	joined := strings.Join(hintsAt(status, now), "\n")
	if !strings.Contains(joined, "boot takes ~1 minute") {
		t.Fatalf("hints = %q, want the calm hint when no start time is known", joined)
	}
}

func TestLogNodeStallsLogsOnceOnStallAndOnceOnRecovery(t *testing.T) {
	var buffer bytes.Buffer
	previous := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(previous); log.SetFlags(flags) })

	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	started := now.Add(-5 * time.Minute)
	service := &Server{}
	statuses := []ClusterStatus{unreachableNodeStatus("qa-dom-worker-1", started)}

	service.logNodeStalls(statuses, now)
	service.logNodeStalls(statuses, now.Add(30*time.Second))
	if got := capturedLogLines(buffer.String()); len(got) != 1 {
		t.Fatalf("stall log = %q, want exactly one entry", buffer.String())
	}
	if !strings.Contains(buffer.String(), "has not answered") {
		t.Fatalf("stall log = %q, want it to say the node has not answered", buffer.String())
	}

	buffer.Reset()
	recovered := []ClusterStatus{{
		Name:  "qa-dom",
		Nodes: []NodeStatus{{Name: "qa-dom-worker-1", Phase: PhaseMaintenance, StartedAt: &started}},
	}}
	service.logNodeStalls(recovered, now.Add(time.Minute))
	service.logNodeStalls(recovered, now.Add(2*time.Minute))
	if got := capturedLogLines(buffer.String()); len(got) != 1 {
		t.Fatalf("recovery log = %q, want exactly one entry", buffer.String())
	}
	if !strings.Contains(buffer.String(), "answered") {
		t.Fatalf("recovery log = %q, want a recovery entry", buffer.String())
	}
}

func TestLogNodeStallsStaysSilentInsideTheBootWindow(t *testing.T) {
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	t.Cleanup(func() { log.SetOutput(previous) })

	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	service := &Server{}
	service.logNodeStalls([]ClusterStatus{unreachableNodeStatus("qa-dom-worker-1", now.Add(-30*time.Second))}, now)
	if buffer.Len() != 0 {
		t.Fatalf("log = %q, want silence inside the boot window", buffer.String())
	}
}

func capturedLogLines(output string) []string {
	trimmed := strings.TrimSuffix(output, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}
