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
