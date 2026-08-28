package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/daemon"
)

func TestTalosServicesFindingsCoverPassWarnFailAndSkip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	succeeded := daemon.ServiceProbe{Status: daemon.ServiceProbeSucceeded}
	missing := daemon.ServiceProbe{Status: daemon.ServiceProbeMissingCredentials}
	failed := daemon.ServiceProbe{Status: daemon.ServiceProbeFailed, Error: "authentication failed"}
	tests := []struct {
		name       string
		statuses   []daemon.ClusterStatus
		statusErr  error
		clusterErr error
		levels     []string
		contains   []string
	}{
		{name: "skip without configured node", statuses: []daemon.ClusterStatus{{Name: "stopped"}}, levels: []string{"SKIP"}},
		{name: "skip when daemon is unavailable", clusterErr: dialError{err: errors.New("connection refused")}, levels: []string{"SKIP"}, contains: []string{"daemon unavailable: connection refused"}},
		{name: "skip when cluster listing fails", clusterErr: errors.New("decode daemon result"), levels: []string{"SKIP"}, contains: []string{"list clusters: decode daemon result"}},
		{name: "warn when status unavailable", statusErr: errors.New("daemon timed out"), levels: []string{"WARN"}, contains: []string{"daemon timed out"}},
		{name: "pass after successful inspection", statuses: []daemon.ClusterStatus{{Name: "demo", Running: true, Nodes: []daemon.NodeStatus{{Name: "demo-cp-1", Phase: daemon.PhaseConfigured, ServiceProbe: &succeeded}}}}, levels: []string{"PASS"}},
		{name: "warn for missing credentials and probe errors", statuses: []daemon.ClusterStatus{{Name: "demo", Running: true, Nodes: []daemon.NodeStatus{
			{Name: "demo-cp-1", Phase: daemon.PhaseConfigured, ServiceProbe: &missing},
			{Name: "demo-worker-1", Phase: daemon.PhaseConfigured, ServiceProbe: &failed},
		}}}, levels: []string{"WARN", "WARN"}, contains: []string{"demo/demo-cp-1", "missing", "demo/demo-worker-1", "authentication failed"}},
		{name: "fail every stalled and terminal service", statuses: []daemon.ClusterStatus{{Name: "demo", Running: true, Nodes: []daemon.NodeStatus{
			{Name: "demo-cp-1", Phase: daemon.PhaseConfigured, ServiceProbe: &succeeded, StalledServices: []daemon.StalledService{{Service: "etcd", State: "Starting", Since: now.Add(-5 * time.Minute)}}},
			{Name: "demo-worker-1", Phase: daemon.PhaseConfigured, ServiceProbe: &succeeded, Services: []daemon.NodeService{{Name: "kubelet", State: "Failed", Health: daemon.ServiceHealthCrashLooping}}},
		}}}, levels: []string{"FAIL", "FAIL"}, contains: []string{"demo/demo-cp-1", "etcd Starting", "demo/demo-worker-1", "kubelet Failed"}},
		{name: "ignore transient failed state", statuses: []daemon.ClusterStatus{{Name: "demo", Running: true, Nodes: []daemon.NodeStatus{{Name: "demo-cp-1", Phase: daemon.PhaseConfigured, ServiceProbe: &succeeded, Services: []daemon.NodeService{{Name: "kubelet", State: "Failed", Health: daemon.ServiceHealthStarting}}}}}}, levels: []string{"PASS"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			findings := talosServicesFindings(tt.statuses, tt.statusErr, tt.clusterErr, now)
			if len(findings) != len(tt.levels) {
				t.Fatalf("findings = %+v, want levels %v", findings, tt.levels)
			}
			joined := ""
			for i, finding := range findings {
				if finding.check != "talos-services" || finding.level != tt.levels[i] {
					t.Fatalf("finding[%d] = %+v, want %s talos-services", i, finding, tt.levels[i])
				}
				joined += finding.detail + "\n"
			}
			for _, want := range tt.contains {
				if !strings.Contains(joined, want) {
					t.Fatalf("findings missing %q:\n%s", want, joined)
				}
			}
		})
	}
}
