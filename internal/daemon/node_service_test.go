package daemon

import (
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// stubNodeService replaces the live machine-API probe for one test. Nothing in
// the package may dial a node, so every test that exercises the service surface
// installs its own reading.
func stubNodeService(t *testing.T, probe func(clusterName, ip, service string) (NodeService, bool)) {
	t.Helper()
	previous := probeNodeService
	probeNodeService = probe
	t.Cleanup(func() { probeNodeService = previous })
}

// TestClassifyService pins the rules that turn one machine-API observation into
// the word status prints (#357).
func TestClassifyService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		observation ServiceObservation
		want        ServiceHealth
	}{
		{
			name:        "no state at all is unknown",
			observation: ServiceObservation{HealthUnknown: true},
			want:        ServiceHealthUnknown,
		},
		{
			name:        "running and healthy",
			observation: ServiceObservation{State: "Running", Healthy: true},
			want:        ServiceHealthHealthy,
		},
		{
			name:        "running and healthy despite earlier failures",
			observation: ServiceObservation{State: "Running", Healthy: true, Failures: 5},
			want:        ServiceHealthHealthy,
		},
		{
			name:        "failed state is a crash loop",
			observation: ServiceObservation{State: "Failed", HealthUnknown: true},
			want:        ServiceHealthCrashLooping,
		},
		{
			name:        "repeated failure events are a crash loop even mid-restart",
			observation: ServiceObservation{State: "Preparing", HealthUnknown: true, Failures: crashLoopFailures},
			want:        ServiceHealthCrashLooping,
		},
		{
			name:        "a single restart is still starting",
			observation: ServiceObservation{State: "Preparing", HealthUnknown: true, Failures: 1},
			want:        ServiceHealthStarting,
		},
		{
			name:        "running without a verdict is unknown",
			observation: ServiceObservation{State: "Running", HealthUnknown: true},
			want:        ServiceHealthUnknown,
		},
		{
			name:        "running with a failing health probe is unhealthy",
			observation: ServiceObservation{State: "Running"},
			want:        ServiceHealthUnhealthy,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyService(kubeletService, tt.observation)
			if got.Health != tt.want {
				t.Fatalf("classifyService(%+v).Health = %q, want %q", tt.observation, got.Health, tt.want)
			}
			if got.Name != kubeletService || got.State != tt.observation.State {
				t.Fatalf("classifyService lost the observation: %+v", got)
			}
		})
	}
}

// TestClassifyServiceKeepsTheDiagnosisShort keeps one node's message from
// taking over the hint it lands in.
func TestClassifyServiceKeepsTheDiagnosisShort(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", serviceMessageMaxLen*2)
	got := classifyService(kubeletService, ServiceObservation{State: "Failed", Message: long})
	if len(got.Message) > serviceMessageMaxLen+len("…") {
		t.Fatalf("message not truncated: %d chars", len(got.Message))
	}
	if !strings.HasSuffix(got.Message, "…") {
		t.Fatalf("truncated message does not say so: %q", got.Message)
	}
}

// TestRefreshNodeStatusesProbesConfiguredNodesOnly keeps the probe
// proportionate: a stopped or unreachable node has no machine API to ask, and
// the stall sweep's statuses (Running unset) must stay probe-free.
func TestRefreshNodeStatusesProbesConfiguredNodesOnly(t *testing.T) {
	asked := make(chan string, 8)
	stubNodeService(t, func(_, ip, service string) (NodeService, bool) {
		asked <- ip + "/" + service
		return classifyService(service, ServiceObservation{State: "Running", Healthy: true}), true
	})

	service := &Server{
		vms:          map[string]map[string]hypervisor.Machine{},
		nodeIPLookup: func(mac string, _ int) string { return "172.30.0." + mac },
		nodeProbe: func(ip string) ProbeResult {
			return ProbeResult{Dialed: ip == "172.30.0.2", TLS: ip == "172.30.0.2"}
		},
	}
	statuses := []ClusterStatus{{
		Name:    "demo",
		Running: true,
		Nodes: []NodeStatus{
			{Name: "demo-cp-1", Role: cluster.RoleControlPlane, MAC: "2", Phase: PhaseConfigured},
			{Name: "demo-worker-1", Role: cluster.RoleWorker, MAC: "3", Phase: PhaseConfigured},
		},
	}}
	service.refreshNodeStatuses(statuses)
	close(asked)

	var probed []string
	for entry := range asked {
		probed = append(probed, entry)
	}
	if len(probed) != 1 || probed[0] != "172.30.0.2/kubelet" {
		t.Fatalf("kubelet probes = %v, want only the configured node", probed)
	}
	if statuses[0].Nodes[0].Kubelet == nil || statuses[0].Nodes[0].Kubelet.Health != ServiceHealthHealthy {
		t.Fatalf("configured node carries no kubelet reading: %+v", statuses[0].Nodes[0])
	}
	if statuses[0].Nodes[1].Kubelet != nil {
		t.Fatalf("unreachable node reports a kubelet reading: %+v", statuses[0].Nodes[1].Kubelet)
	}
}

// TestRefreshNodeServicesSkipsAStoppedCluster pins the gate the periodic stall
// sweep relies on: its synthetic statuses leave Running unset, and a sweep that
// dialled every node's machine API every tick would not be proportionate.
func TestRefreshNodeServicesSkipsAStoppedCluster(t *testing.T) {
	stubNodeService(t, func(string, string, string) (NodeService, bool) {
		t.Fatal("probed a cluster that is not running")
		return NodeService{}, false
	})
	status := ClusterStatus{Name: "demo", Nodes: []NodeStatus{{Name: "demo-cp-1", IP: "172.30.0.2", Phase: PhaseConfigured}}}
	refreshNodeServices(&status)
}

// TestRefreshNodeStatusesKeepsSuspendedNodes pins #360: the refresh re-derives
// the live phase, but the suspension it inherits is a disk fact — dropping it
// made a suspended cluster read as plain stopped in the PHASE column.
func TestRefreshNodeStatusesKeepsSuspendedNodes(t *testing.T) {
	service := &Server{
		vms:          map[string]map[string]hypervisor.Machine{},
		nodeIPLookup: func(string, int) string { return "" },
		nodeProbe:    func(string) ProbeResult { return ProbeResult{} },
	}
	statuses := []ClusterStatus{{
		Name:      "napping",
		Suspended: true,
		Nodes: []NodeStatus{
			{Name: "napping-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseStopped, Suspended: true},
			{Name: "napping-worker-1", Role: cluster.RoleWorker, Phase: PhaseStopped},
		},
	}}
	service.refreshNodeStatuses(statuses)

	if !statuses[0].Nodes[0].Suspended {
		t.Fatal("the node holding saved memory lost its suspended flag in the refresh")
	}
	if statuses[0].Nodes[1].Suspended {
		t.Fatal("a node without saved memory gained a suspended flag")
	}
}
