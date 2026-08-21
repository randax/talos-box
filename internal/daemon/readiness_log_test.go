package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
)

// unresumableStatus is the shape #418 escalated on: a converged cluster with no
// talosbox.yaml behind it, seen during a momentary apiserver blip.
func unresumableStatus() ClusterStatus {
	return ClusterStatus{
		Name:               "qa-host",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
		Running:            true,
		ConfigOrigin:       cluster.OriginImperative,
		Nodes: []NodeStatus{
			{Name: "qa-host-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"},
		},
	}
}

// TestBlipDoesNotEscalateToDestroy pins #418: a single unreachable observation
// recommended destroying a healthy cluster. Destruction is the most expensive
// advice status gives, so it waits for the condition to persist and names the
// cheap moves in the meantime.
func TestBlipDoesNotEscalateToDestroy(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name        string
		unreadyFor  time.Duration
		observed    bool
		wantDestroy bool
	}{
		{name: "momentary blip", unreadyFor: 20 * time.Second, observed: true},
		{name: "still inside the debounce", unreadyFor: unreadyEscalationWindow - time.Second, observed: true},
		{name: "persistently stuck", unreadyFor: unreadyEscalationWindow + time.Minute, observed: true, wantDestroy: true},
		// No observation window at all (an older daemon, a status built
		// outside the readiness log) keeps the stuck-cluster advice: the
		// daemon cannot vouch for the cluster either way.
		{name: "no observation window", wantDestroy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := unresumableStatus()
			if test.observed {
				since := now.Add(-test.unreadyFor)
				status.KubernetesNotReadySince = &since
			}
			got := provisioningRecoveryHintAt(status, now)
			if destroyed := strings.Contains(got, "tbx cluster destroy qa-host --force"); destroyed != test.wantDestroy {
				t.Fatalf("destroy recommended = %t, want %t: %s", destroyed, test.wantDestroy, got)
			}
			if test.wantDestroy {
				return
			}
			for _, want := range []string{"transient control-plane blip", "rerun tbx status qa-host", "tbxd.log"} {
				if !strings.Contains(got, want) {
					t.Fatalf("transient hint missing %q: %s", want, got)
				}
			}
		})
	}
}

// TestReadinessLogTracksTheFirstFailure keeps the debounce anchored to the
// start of the current run of failures, not to the latest observation.
func TestReadinessLogTracksTheFirstFailure(t *testing.T) {
	var log readinessLog
	start := time.Now()
	first := log.observe("qa-host", false, start)
	if first == nil || !first.Equal(start) {
		t.Fatalf("first failure = %v, want %v", first, start)
	}
	later := log.observe("qa-host", false, start.Add(time.Minute))
	if later == nil || !later.Equal(start) {
		t.Fatalf("second failure = %v, want the first failure's time %v", later, start)
	}
	if recovered := log.observe("qa-host", true, start.Add(2*time.Minute)); recovered != nil {
		t.Fatalf("a ready cluster reported unreadiness since %v", recovered)
	}
	if again := log.observe("qa-host", false, start.Add(3*time.Minute)); again == nil || !again.Equal(start.Add(3*time.Minute)) {
		t.Fatalf("failure after a recovery = %v, want a fresh clock", again)
	}
}

// TestObserveKubernetesReadinessStampsOnlyProvisionedRunningClusters keeps the
// stamp off clusters that have no readiness verdict to fail.
func TestObserveKubernetesReadinessStampsOnlyProvisionedRunningClusters(t *testing.T) {
	now := time.Now()
	service := &Server{}
	statuses := []ClusterStatus{
		{Name: "substrate", Running: true},
		{Name: "stopped", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}},
		{Name: "blipping", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}, Running: true},
	}
	service.observeKubernetesReadiness(statuses, now)
	if statuses[0].KubernetesNotReadySince != nil || statuses[1].KubernetesNotReadySince != nil {
		t.Fatalf("stamped a cluster with no readiness verdict: %+v", statuses)
	}
	if statuses[2].KubernetesNotReadySince == nil {
		t.Fatal("a running provisioned cluster failing readiness was not stamped")
	}
}

// TestReadinessLogStartsFreshRunAfterAnUnwatchedGap pins the staleness rule:
// observations only happen when a client polls, so a cluster can fail, recover,
// and fail again between two polls. A gap the daemon did not watch is not
// evidence that the failure persisted through it, and inheriting the old clock
// let a brand-new blip escalate straight to the #418 destroy advice.
func TestReadinessLogStartsFreshRunAfterAnUnwatchedGap(t *testing.T) {
	var log readinessLog
	start := time.Now()
	if first := log.observe("qa-host", false, start); first == nil || !first.Equal(start) {
		t.Fatalf("first failure = %v, want %v", first, start)
	}
	// The cluster recovers with nobody polling, then blips again much later.
	gap := start.Add(unreadyRunAbandonWindow + time.Minute)
	again := log.observe("qa-host", false, gap)
	if again == nil || !again.Equal(gap) {
		t.Fatalf("failure after an unwatched gap = %v, want a fresh clock %v", again, gap)
	}
	// Inside the window the run still accumulates, so a genuinely stuck
	// cluster still escalates.
	within := gap.Add(unreadyRunAbandonWindow - time.Second)
	if run := log.observe("qa-host", false, within); run == nil || !run.Equal(gap) {
		t.Fatalf("failure inside the window = %v, want the run's start %v", run, gap)
	}
}

// TestBlipAfterAnUnwatchedRecoveryDoesNotEscalate walks the whole path #418
// cares about: two unrelated blips straddling a poll gap must not add up to a
// destroy recommendation.
func TestBlipAfterAnUnwatchedRecoveryDoesNotEscalate(t *testing.T) {
	service := &Server{}
	start := time.Now()
	blip := func(now time.Time) string {
		statuses := []ClusterStatus{unresumableStatus()}
		service.observeKubernetesReadiness(statuses, now)
		return provisioningRecoveryHintAt(statuses[0], now)
	}
	if got := blip(start); strings.Contains(got, "tbx cluster destroy") {
		t.Fatalf("first blip escalated to destroy: %s", got)
	}
	if got := blip(start.Add(unreadyRunAbandonWindow + time.Minute)); strings.Contains(got, "tbx cluster destroy") {
		t.Fatalf("a fresh blip after an unwatched recovery escalated to destroy: %s", got)
	}
}

// TestSlowPollerStillEscalates pins the other half of the staleness rule: the
// abandon window is not the escalation window. An operator (or a script)
// polling less often than the 2-minute escalation window used to restart the
// run on every poll, so a dead cluster could never reach the destroy advice
// (#418).
func TestSlowPollerStillEscalates(t *testing.T) {
	service := &Server{}
	start := time.Now()
	poll := func(now time.Time) string {
		statuses := []ClusterStatus{unresumableStatus()}
		service.observeKubernetesReadiness(statuses, now)
		return provisioningRecoveryHintAt(statuses[0], now)
	}
	// A poll interval longer than the escalation window but shorter than the
	// abandon window: the run accumulates across the gaps.
	interval := unreadyEscalationWindow + time.Minute
	if got := poll(start); strings.Contains(got, "tbx cluster destroy") {
		t.Fatalf("the first observation escalated to destroy: %s", got)
	}
	if got := poll(start.Add(interval)); !strings.Contains(got, "tbx cluster destroy qa-host --force") {
		t.Fatalf("a cluster unready across a %s poll gap did not escalate: %s", interval, got)
	}
}
