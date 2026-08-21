package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
)

// convergedStatus is a cluster whose nodes are up, whose Kubernetes reports
// Ready and whose VIP answers: the reading a single-sample check may treat as
// converged.
func convergedStatus(now time.Time) ClusterStatus {
	booted := now.Add(-time.Hour)
	return ClusterStatus{
		Name:               "qa-snap",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn, LB: true},
		Running:            true,
		KubernetesReady:    true,
		StoragePhase:       StoragePhaseLive,
		VIP:                "172.30.0.200",
		VIPLive:            true,
		Nodes: []NodeStatus{
			{Name: "qa-snap-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2", StartedAt: &booted},
		},
	}
}

// TestConvergingReasonsSeparateSettlingFromConverged pins #396 and #427: nodes
// Ready is not convergence. Each reason is derived from a fact status already
// holds, so naming them costs no extra probe.
func TestConvergingReasonsSeparateSettlingFromConverged(t *testing.T) {
	now := time.Now()
	freshBoot := now.Add(-30 * time.Second)
	for _, test := range []struct {
		name    string
		mutate  func(*ClusterStatus)
		wants   []string
		settled bool
	}{
		{name: "fully converged", mutate: func(*ClusterStatus) {}, settled: true},
		{
			name:   "CSI drivers still registering",
			mutate: func(s *ClusterStatus) { s.StoragePhase = StoragePhaseProvisioning },
			wants:  []string{"longhorn CSI drivers have not passed the readiness probe", "PVC mounts can fail"},
		},
		{
			name:   "VIP announced but not answering",
			mutate: func(s *ClusterStatus) { s.VIPLive = false },
			wants:  []string{"172.30.0.200 is announced but not answering yet"},
		},
		{
			name:   "still inside the boot settle window",
			mutate: func(s *ClusterStatus) { s.Nodes[0].StartedAt = &freshBoot },
			wants:  []string{"kubelet serving certificates", "CSI driver registrations"},
		},
		{
			name:    "a stopped cluster is not settling",
			mutate:  func(s *ClusterStatus) { s.Running = false; s.StoragePhase = "" },
			settled: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := convergedStatus(now)
			test.mutate(&status)
			reasons := convergingReasons(status, now)
			if test.settled {
				if len(reasons) != 0 {
					t.Fatalf("convergingReasons() = %q, want none", reasons)
				}
				return
			}
			joined := strings.Join(reasons, "; ")
			for _, want := range test.wants {
				if !strings.Contains(joined, want) {
					t.Fatalf("convergingReasons() = %q, missing %q", joined, want)
				}
			}
		})
	}
}

// TestHintsNameWhatIsStillSettling keeps the reasons visible where the operator
// reads them, next to the "Kubernetes is Ready" line that used to stand alone.
func TestHintsNameWhatIsStillSettling(t *testing.T) {
	now := time.Now()
	status := convergedStatus(now)
	status.StoragePhase = StoragePhaseProvisioning
	joined := strings.Join(hintsAt(status, now), "\n")
	if !strings.Contains(joined, "still settling") {
		t.Fatalf("hints do not name the settling window:\n%s", joined)
	}
	if !strings.Contains(joined, "longhorn CSI drivers") {
		t.Fatalf("hints do not name what is coming back:\n%s", joined)
	}
}

// TestHintsDistinguishAnnouncedVIPFromLiveVIP pins #427's expected surface: the
// address is reported through the window, so a single-sample check can tell
// "announced but not answering" from "no VIP at all".
func TestHintsDistinguishAnnouncedVIPFromLiveVIP(t *testing.T) {
	now := time.Now()
	status := convergedStatus(now)
	status.VIPLive = false
	joined := strings.Join(hintsAt(status, now), "\n")
	for _, want := range []string{"172.30.0.200 is announced but not answering yet", "Cilium LB-IPAM"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("hints missing %q:\n%s", want, joined)
		}
	}

	status.VIP = ""
	joined = strings.Join(hintsAt(status, now), "\n")
	if !strings.Contains(joined, "waiting for the Cilium LB-IPAM LoadBalancer VIP to be announced") {
		t.Fatalf("hints do not name the un-announced VIP:\n%s", joined)
	}
}
