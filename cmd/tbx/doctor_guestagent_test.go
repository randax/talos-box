package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
)

func TestRunDoctorReportsGuestAgentCapabilityGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		clusters []cluster.Cluster
		listErr  error
		support  hypervisor.FeatureStatus
		want     string
	}{
		{
			name:     "gated host",
			clusters: []cluster.Cluster{{Name: "demo", TalosExtensions: []string{"qemu-guest-agent"}}},
			support:  hypervisor.FeatureStatus{Reason: "backend has no guest-agent channel"},
			want:     "WARN guest-agent: cluster(s) demo request qemu-guest-agent: backend has no guest-agent channel",
		},
		{
			name:     "capable host",
			clusters: []cluster.Cluster{{Name: "demo", TalosExtensions: []string{"qemu-guest-agent"}}},
			support:  hypervisor.FeatureStatus{Supported: true},
			want:     "PASS guest-agent: channel available for cluster(s) demo",
		},
		{
			name:     "no cluster requests it",
			clusters: []cluster.Cluster{{Name: "demo", TalosExtensions: []string{"gvisor"}}},
			support:  hypervisor.FeatureStatus{Reason: "backend has no guest-agent channel"},
			want:     "SKIP guest-agent: no cluster requests qemu-guest-agent",
		},
		{
			name:    "cluster state unreadable",
			listErr: errors.New("state unreadable"),
			support: hypervisor.FeatureStatus{Supported: true},
			want:    "SKIP guest-agent: cluster state unavailable: state unreadable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			deps := guestAgentDoctorDependencies()
			deps.listConfig = func() ([]cluster.Cluster, error) { return test.clusters, test.listErr }
			deps.guestAgentSupport = func() hypervisor.FeatureStatus { return test.support }

			var output strings.Builder
			if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
				t.Fatalf("runDoctorWithDependencies() = %v, want a non-fatal capability report", err)
			}
			if !strings.Contains(output.String(), test.want) {
				t.Fatalf("output = %q, want it to contain %q", output.String(), test.want)
			}
		})
	}
}

func guestAgentDoctorDependencies() doctorDependencies {
	pass := func() error { return nil }
	return doctorDependencies{
		checkHelper:     pass,
		checkResolver:   pass,
		checkDirectDNS:  pass,
		checkForwarding: pass,
		listClusters:    func() ([]daemon.ClusterSummary, error) { return nil, nil },
		getStatus:       func() ([]daemon.ClusterStatus, error) { return nil, nil },
		listCache:       func() (daemon.CacheListResult, error) { return daemon.CacheListResult{}, nil },
		hostPressure:    func() (hostpressure.Snapshot, error) { return hostpressure.Snapshot{}, nil },
		command:         func(string, ...string) ([]byte, error) { return nil, nil },
		doHTTP:          func(*http.Request) (*http.Response, error) { return &http.Response{Body: http.NoBody}, nil },
	}
}
