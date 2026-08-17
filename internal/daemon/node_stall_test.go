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

// A node that answered for hours and only just went quiet is not a stalled
// boot: the stall clock starts when it stopped answering, not when it launched.
func TestHintsStayCalmWhenHealthyNodeJustStoppedAnswering(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	started := now.Add(-2 * time.Hour)
	since := now.Add(-30 * time.Second)
	status := ClusterStatus{
		Name:  "qa-dom",
		Nodes: []NodeStatus{{Name: "qa-dom-worker-1", Phase: PhaseUnreachable, StartedAt: &started, UnreachableSince: &since}},
	}
	joined := strings.Join(hintsAt(status, now), "\n")
	if !strings.Contains(joined, "boot takes ~1 minute") {
		t.Fatalf("hints = %q, want the calm hint 30s after the node went quiet", joined)
	}
	if strings.Contains(joined, "tbx console") {
		t.Fatalf("hints = %q, want no escalation from VM uptime", joined)
	}
}

func TestHintsMeasureStallFromWhenTheNodeStoppedAnswering(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	started := now.Add(-2 * time.Hour)
	since := now.Add(-4 * time.Minute)
	status := ClusterStatus{
		Name:  "qa-dom",
		Nodes: []NodeStatus{{Name: "qa-dom-worker-1", Phase: PhaseUnreachable, StartedAt: &started, UnreachableSince: &since}},
	}
	joined := strings.Join(hintsAt(status, now), "\n")
	if !strings.Contains(joined, "stopped answering 4m0s ago") {
		t.Fatalf("hints = %q, want the age measured from the unreachable transition", joined)
	}
	for _, unwanted := range []string{"since its VM started", "2h"} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("hints = %q, want no VM-uptime claim %q", joined, unwanted)
		}
	}
}

func TestHintsKeepVMStartWordingForNodeThatNeverAnswered(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	joined := strings.Join(hintsAt(unreachableNodeStatus("qa-dom-worker-1", now.Add(-6*time.Minute)), now), "\n")
	if !strings.Contains(joined, "has not answered for 6m0s since its VM started") {
		t.Fatalf("hints = %q, want the boot-stall wording for a node that never answered", joined)
	}
}

func TestLogNodeStallsMeasuresFromTheUnreachableTransition(t *testing.T) {
	var buffer bytes.Buffer
	previous := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(previous); log.SetFlags(flags) })

	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	started := now.Add(-2 * time.Hour)
	since := now.Add(-4 * time.Minute)
	service := &Server{}
	statuses := []ClusterStatus{{
		Name:  "qa-dom",
		Nodes: []NodeStatus{{Name: "qa-dom-worker-1", Phase: PhaseUnreachable, StartedAt: &started, UnreachableSince: &since}},
	}}
	service.logNodeStalls(statuses, now)
	logged := buffer.String()
	if !strings.Contains(logged, "stopped answering on apid 4m0s ago") {
		t.Fatalf("stall log = %q, want the transition-based age", logged)
	}
	if strings.Contains(logged, "since its VM started") || strings.Contains(logged, "2h") {
		t.Fatalf("stall log = %q, want no VM-uptime claim", logged)
	}

	buffer.Reset()
	recovered := []ClusterStatus{{
		Name:  "qa-dom",
		Nodes: []NodeStatus{{Name: "qa-dom-worker-1", Phase: PhaseConfigured, StartedAt: &started}},
	}}
	service.logNodeStalls(recovered, now)
	if !strings.Contains(buffer.String(), "after 4m0s unreachable") {
		t.Fatalf("recovery log = %q, want the same clock as the stall line", buffer.String())
	}
}

func TestObserveReachabilityStartsTheClockOnlyAfterTheNodeAnswered(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	service := &Server{}

	// Never answered since launch: StartedAt stays the only clock.
	if since := service.reachability.observe("qa-dom/w1", PhaseUnreachable, now); since != nil {
		t.Fatalf("unreachableSince = %v, want nil for a node that never answered", since)
	}
	// It answers, then goes quiet: the clock starts at the first quiet poll.
	service.reachability.observe("qa-dom/w1", PhaseConfigured, now.Add(time.Minute))
	transition := now.Add(2 * time.Minute)
	since := service.reachability.observe("qa-dom/w1", PhaseUnreachable, transition)
	if since == nil || !since.Equal(transition) {
		t.Fatalf("unreachableSince = %v, want %v", since, transition)
	}
	// A later poll keeps the original transition time.
	if again := service.reachability.observe("qa-dom/w1", PhaseUnreachable, now.Add(9*time.Minute)); again == nil || !again.Equal(transition) {
		t.Fatalf("unreachableSince = %v, want the first transition %v", again, transition)
	}
	// Stopping the VM resets it: the next boot is aged from its own start.
	service.reachability.observe("qa-dom/w1", PhaseStopped, now.Add(10*time.Minute))
	if since := service.reachability.observe("qa-dom/w1", PhaseUnreachable, now.Add(11*time.Minute)); since != nil {
		t.Fatalf("unreachableSince = %v, want nil after the VM stopped", since)
	}
}

func TestRecordVMStartResetsTheReachabilityClock(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	service := &Server{}
	service.reachability.observe("qa-dom/w1", PhaseConfigured, now)
	service.recordVMStart("qa-dom", "w1")
	if since := service.reachability.observe("qa-dom/w1", PhaseUnreachable, now.Add(time.Minute)); since != nil {
		t.Fatalf("unreachableSince = %v, want nil after a relaunch", since)
	}
}

func TestRefreshNodeStatusesStampsUnreachableSinceAfterTheNodeAnswered(t *testing.T) {
	reachable := true
	service := &Server{
		nodeIPLookup: func(string, int) string { return "192.0.2.10" },
		nodeProbe: func(string) ProbeResult {
			if reachable {
				return ProbeResult{Dialed: true, TLS: true}
			}
			return ProbeResult{}
		},
	}
	statuses := []ClusterStatus{{Name: "qa-dom", Nodes: []NodeStatus{{Name: "qa-dom-worker-1", Phase: PhaseConfigured}}}}
	service.refreshNodeStatuses(statuses)
	if statuses[0].Nodes[0].UnreachableSince != nil {
		t.Fatalf("unreachableSince = %v, want nil while the node answers", statuses[0].Nodes[0].UnreachableSince)
	}
	reachable = false
	service.refreshNodeStatuses(statuses)
	if statuses[0].Nodes[0].UnreachableSince == nil {
		t.Fatal("refresh did not stamp the unreachable transition")
	}
}

func TestForgetNodeAndClusterPruneStallTracking(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	service := &Server{}
	service.recordVMStart("qa-dom", "w1")
	service.recordVMStart("qa-dom", "w2")
	service.reachability.observe("qa-dom/w1", PhaseConfigured, now)
	service.reachability.observe("qa-dom/w2", PhaseConfigured, now)
	service.stalls.observe("qa-dom/w1", true, time.Minute)
	service.stalls.observe("qa-dom/w2", true, time.Minute)

	service.forgetNode("qa-dom", "w1")
	if _, ok := service.vmStarts["qa-dom"]["w1"]; ok {
		t.Fatal("vmStarts kept a removed node")
	}
	if _, ok := service.reachability.nodes["qa-dom/w1"]; ok {
		t.Fatal("reachability kept a removed node")
	}
	if _, ok := service.stalls.stalls["qa-dom/w1"]; ok {
		t.Fatal("stall log kept a removed node")
	}
	if _, ok := service.vmStarts["qa-dom"]["w2"]; !ok {
		t.Fatal("forgetNode pruned a sibling node")
	}

	service.forgetCluster("qa-dom")
	if _, ok := service.vmStarts["qa-dom"]; ok {
		t.Fatal("vmStarts kept a destroyed cluster")
	}
	if len(service.reachability.nodes) != 0 || len(service.stalls.stalls) != 0 {
		t.Fatalf("cluster tracking survived: %v %v", service.reachability.nodes, service.stalls.stalls)
	}
}

func TestForgetAllNodeTrackingClearsEverything(t *testing.T) {
	service := &Server{}
	service.recordVMStart("qa-dom", "w1")
	service.reachability.observe("qa-dom/w1", PhaseConfigured, time.Now())
	service.stalls.observe("qa-dom/w1", true, time.Minute)
	service.forgetAllNodeTracking()
	if len(service.vmStarts) != 0 || len(service.reachability.nodes) != 0 || len(service.stalls.stalls) != 0 {
		t.Fatal("shutdown left node stall tracking behind")
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
