package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
)

func TestRefreshKubernetesReadinessDoesNotClaimReadyWithoutCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	statuses := []ClusterStatus{{
		Running: true,
		Name:    "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
		Nodes: []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}},
	}}
	refreshKubernetesReadiness(statuses)
	if statuses[0].KubernetesReady {
		t.Fatal("status claimed Kubernetes Ready without a valid kubeconfig")
	}
	for _, hint := range statuses[0].Hints {
		if strings.Contains(hint, "Kubernetes is Ready") {
			t.Fatalf("status emitted an unverified Ready hint: %q", hint)
		}
	}
}

func TestRefreshKubernetesReadinessSkipsStoppedFlannelClusters(t *testing.T) {
	statuses := []ClusterStatus{{
		Name:               "demo",
		Running:            false,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		KubernetesReady:    true,
		VIP:                "172.30.0.200",
		VIPLive:            true,
		Nodes:              []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseStopped, IP: "172.30.0.2"}},
	}}

	refreshKubernetesReadiness(statuses)

	if statuses[0].KubernetesReady {
		t.Fatal("stopped cluster reported Kubernetes ready")
	}
	if statuses[0].VIP != "" || statuses[0].VIPLive {
		t.Fatalf("stopped cluster retained stale VIP state: %+v", statuses[0])
	}
}

func TestProvisioningCompleteUsesMultiSecondProbeBudgets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secrets.yaml", "talosconfig", "kubeconfig"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	originalReady := kubernetesReadyProbe
	originalCilium := ciliumConvergenceProbe
	t.Cleanup(func() {
		kubernetesReadyProbe = originalReady
		ciliumConvergenceProbe = originalCilium
	})
	remaining := func(ctx context.Context) time.Duration {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("probe context has no deadline")
		}
		return time.Until(deadline)
	}
	kubernetesReadyProbe = func(ctx context.Context, _ []byte, _ []string) error {
		if got := remaining(ctx); got < 4*time.Second {
			t.Errorf("Kubernetes Ready budget = %s, want at least 4s", got)
		}
		return nil
	}
	ciliumConvergenceProbe = func(ctx context.Context, _ []byte, _ cluster.Cluster) error {
		if got := remaining(ctx); got < 14*time.Second {
			t.Errorf("Cilium convergence budget = %s, want at least 14s", got)
		}
		return nil
	}

	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}}
	if !(&Server{}).provisioningComplete(item) {
		t.Fatal("healthy Cilium cluster was not considered complete")
	}
}

func TestProvisioningCompleteCiliumRequiresLiveVIPAndMatchingBGP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secrets.yaml", "talosconfig", "kubeconfig"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a kubeconfig"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	originalReady := kubernetesReadyProbe
	originalCilium := ciliumConvergenceProbe
	originalVIP := loadBalancerVIPProbe
	originalHelper := connectBGPHelper
	t.Cleanup(func() {
		kubernetesReadyProbe = originalReady
		ciliumConvergenceProbe = originalCilium
		loadBalancerVIPProbe = originalVIP
		connectBGPHelper = originalHelper
	})
	kubernetesReadyProbe = func(context.Context, []byte, []string) error { return nil }
	ciliumConvergenceProbe = func(context.Context, []byte, cluster.Cluster) error { return nil }

	tests := []struct {
		name      string
		vipLive   bool
		wantBGP   bool
		activeBGP bool
		want      bool
	}{
		{name: "VIP not live", want: false},
		{name: "VIP live and BGP disabled", vipLive: true, want: true},
		{name: "VIP live but stale BGP active", vipLive: true, activeBGP: true, want: false},
		{name: "VIP live but BGP inactive", vipLive: true, wantBGP: true, want: false},
		{name: "VIP live and BGP matches", vipLive: true, wantBGP: true, activeBGP: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loadBalancerVIPProbe = func(cluster.Cluster) (string, bool) { return "172.30.0.200", tt.vipLive }
			connectBGPHelper = func() (bgpHelperClient, error) {
				return &fakeBGPClient{active: tt.activeBGP}, nil
			}
			item := cluster.Cluster{
				Name: "demo",
				ProvisioningIntent: cluster.ProvisioningIntent{
					CNI: cluster.CNICilium,
					LB:  true,
					BGP: tt.wantBGP,
				},
			}
			if got := (&Server{}).provisioningComplete(item); got != tt.want {
				t.Fatalf("provisioningComplete() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestProvisioningCompleteEligibleRequiresObservedEndState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		intent      cluster.ProvisioningIntent
		credentials bool
		ready       bool
		vipLive     bool
		hubbleReady bool
		want        bool
	}{
		{name: "flannel without LoadBalancer", intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}, credentials: true, ready: true, want: true},
		{name: "flannel LoadBalancer VIP", intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}, credentials: true, ready: true, vipLive: true, want: true},
		{name: "missing credentials remints", intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}, ready: true},
		{name: "not Ready continues reconciliation", intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}, credentials: true},
		{name: "VIP unavailable continues reconciliation", intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}, credentials: true, ready: true},
		{name: "Cilium skips only after Hubble convergence", intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, Hubble: true}, credentials: true, ready: true, vipLive: true, hubbleReady: true, want: true},
		{name: "Cilium without LoadBalancer has no host BGP dependency", intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}, credentials: true, ready: true, hubbleReady: true, want: true},
		{name: "Cilium Hubble drift continues reconciliation", intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, Hubble: true}, credentials: true, ready: true, vipLive: true},
		{name: "BGP end state is complete before peer reassertion", intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}, credentials: true, ready: true, vipLive: true, hubbleReady: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := provisioningCompleteEligible(test.intent, test.credentials, test.ready, test.vipLive, test.hubbleReady); got != test.want {
				t.Fatalf("provisioningCompleteEligible(%+v, %t, %t, %t, %t) = %t, want %t", test.intent, test.credentials, test.ready, test.vipLive, test.hubbleReady, got, test.want)
			}
		})
	}
}
