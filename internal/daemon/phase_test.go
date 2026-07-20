package daemon

import (
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

func TestClassifyPhase(t *testing.T) {
	tests := []struct {
		name    string
		running bool
		probe   ProbeResult
		want    Phase
	}{
		{"vm stopped", false, ProbeResult{}, PhaseStopped},
		{"running, no answer on apid", true, ProbeResult{Dialed: false}, PhaseUnreachable},
		{"running, apid not speaking TLS", true, ProbeResult{Dialed: true, TLS: false}, PhaseUnreachable},
		{"running, cluster-CA cert", true, ProbeResult{Dialed: true, TLS: true}, PhaseConfigured},
		{"running, maintenance cert", true, ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}, PhaseMaintenance},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyPhase(tt.running, tt.probe); got != tt.want {
				t.Errorf("ClassifyPhase(%v, %+v) = %q, want %q", tt.running, tt.probe, got, tt.want)
			}
		})
	}
}

func TestHints(t *testing.T) {
	base := ClusterStatus{Name: "demo", Subnet: "172.30.0.0/24"}
	node := func(name string, phase Phase) NodeStatus {
		return NodeStatus{Name: name, Phase: phase, IP: "172.30.0.2"}
	}
	tests := []struct {
		name  string
		nodes []NodeStatus
		want  []string // substrings that must each appear in exactly the hint list
	}{
		{
			name:  "maintenance node suggests config workflow",
			nodes: []NodeStatus{node("demo-cp-1", PhaseMaintenance)},
			want:  []string{"talosctl gen config", "apply-config --insecure"},
		},
		{
			name:  "all configured suggests bootstrap",
			nodes: []NodeStatus{node("demo-cp-1", PhaseConfigured)},
			want:  []string{"talosctl", "bootstrap"},
		},
		{
			name:  "stopped cluster suggests start",
			nodes: []NodeStatus{node("demo-cp-1", PhaseStopped)},
			want:  []string{"tbx cluster start demo"},
		},
		{
			name:  "unreachable suggests patience then doctor",
			nodes: []NodeStatus{node("demo-cp-1", PhaseUnreachable)},
			want:  []string{"tbx doctor"},
		},
		{
			name: "mixed phases yield maintenance hint, not bootstrap",
			nodes: []NodeStatus{
				node("demo-cp-1", PhaseConfigured),
				node("demo-worker-1", PhaseMaintenance),
			},
			want: []string{"apply-config --insecure"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := base
			status.Nodes = tt.nodes
			hints := Hints(status)
			for _, substr := range tt.want {
				found := false
				for _, h := range hints {
					if strings.Contains(h, substr) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("hints %q missing substring %q", hints, substr)
				}
			}
		})
	}
	// the gen-config endpoint must name a control plane, not the maintenance worker
	status2 := base
	status2.Nodes = []NodeStatus{
		{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"},
		{Name: "demo-worker-1", Role: cluster.RoleWorker, Phase: PhaseMaintenance, IP: "172.30.0.3"},
	}
	genHintFound := false
	for _, h := range Hints(status2) {
		if strings.Contains(h, "gen config") {
			genHintFound = true
			if !strings.Contains(h, "demo-cp-1.demo.k8s.test") {
				t.Errorf("gen config hint should use the control-plane endpoint, got %q", h)
			}
		}
	}
	if !genHintFound {
		t.Error("expected a gen config hint for the maintenance worker")
	}

	// bootstrap hint must NOT appear while any node is in maintenance
	status := base
	status.Nodes = []NodeStatus{node("a", PhaseConfigured), node("b", PhaseMaintenance)}
	for _, h := range Hints(status) {
		if strings.Contains(h, "bootstrap") {
			t.Errorf("bootstrap hint offered while a node is still in maintenance: %q", h)
		}
	}
}
