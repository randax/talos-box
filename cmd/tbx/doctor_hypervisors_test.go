package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
)

type doctorTestHypervisor struct{ capabilities hypervisor.Capabilities }

func (h doctorTestHypervisor) Launch(context.Context, hypervisor.Spec) (hypervisor.Machine, error) {
	return nil, nil
}
func (h doctorTestHypervisor) Capabilities() hypervisor.Capabilities { return h.capabilities }
func (doctorTestHypervisor) Architecture() hypervisor.Architecture {
	return hypervisor.ArchitectureARM64
}

func TestRunDoctorPrintsOneLinePerHypervisor(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	deps.daemonInfo = func() (daemon.Info, error) {
		return daemon.Info{
			Hypervisors: []daemon.HypervisorInfo{
				{
					Name: "primary", Available: true,
					BalloonReadback: daemon.FeatureStatusInfo{Supported: true},
					GuestAgent:      daemon.FeatureStatusInfo{Reason: "no channel"},
				},
				{Name: "optional", AvailabilityReason: "optional probe failed"},
			},
			DefaultHypervisor:       "primary",
			DefaultHypervisorSource: hypervisor.DefaultSourceCompiled,
		}, nil
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	wantOptional := "INFO Hypervisors: optional: availability=unavailable (optional probe failed); default=no; balloon-readback=unavailable; suspend-survives-restart=unavailable; guest-agent=unavailable"
	wantPrimary := "INFO Hypervisors: primary: availability=available; default=yes (source=compiled); balloon-readback=supported; suspend-survives-restart=unsupported; guest-agent=unsupported (no channel)"
	text := output.String()
	optionalIndex, primaryIndex := strings.Index(text, wantOptional), strings.Index(text, wantPrimary)
	if optionalIndex < 0 || primaryIndex < 0 {
		t.Fatalf("doctor output missing hypervisor lines:\n%s", text)
	}
	if optionalIndex > primaryIndex {
		t.Fatalf("hypervisor lines are not lexically sorted:\n%s", text)
	}
}

func TestRunDoctorReportsHypervisorsWithDaemonDown(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	deps.daemonInfo = func() (daemon.Info, error) {
		return daemon.Info{}, dialError{err: errors.New("connection refused")}
	}
	deps.hypervisors = func(context.Context) hypervisor.Registry {
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary": {
					Hypervisor:   doctorTestHypervisor{capabilities: hypervisor.Capabilities{GuestAgent: hypervisor.FeatureStatus{Supported: true}}},
					Availability: hypervisor.Availability{Available: true},
				},
			},
			Default: hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		}
	}
	deps.listConfig = func() ([]cluster.Cluster, error) {
		return []cluster.Cluster{{Name: "demo", TalosExtensions: []string{"qemu-guest-agent"}}}, nil
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"INFO Hypervisors: primary: availability=available; default=yes (source=compiled)",
		"probed locally; daemon unavailable",
		"guest-agent=supported",
		"PASS guest-agent: channel available for cluster(s) demo",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, output.String())
		}
	}
}

func TestRunDoctorFallsBackWhenDaemonReportsNoHypervisorInventory(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	deps.daemonInfo = func() (daemon.Info, error) { return daemon.Info{}, nil }
	deps.hypervisors = func(context.Context) hypervisor.Registry {
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary": {
					Hypervisor:   doctorTestHypervisor{capabilities: hypervisor.Capabilities{GuestAgent: hypervisor.FeatureStatus{Supported: true}}},
					Availability: hypervisor.Availability{Available: true},
				},
			},
			Default: hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		}
	}
	deps.listConfig = func() ([]cluster.Cluster, error) {
		return []cluster.Cluster{{Name: "demo", TalosExtensions: []string{"qemu-guest-agent"}}}, nil
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"INFO Hypervisors: primary: availability=available; default=yes (source=compiled)",
		"probed locally; daemon does not report hypervisor inventory",
		"PASS guest-agent: channel available for cluster(s) demo",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, output.String())
		}
	}
}

func TestRunDoctorHypervisorInventoryFallsBackWhenDaemonStalls(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	// doctorCall's silence bound surfaces a stalled daemon as a timeout error
	deps.daemonInfo = func() (daemon.Info, error) { return daemon.Info{}, os.ErrDeadlineExceeded }
	deps.hypervisors = func(context.Context) hypervisor.Registry {
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary": {Hypervisor: doctorTestHypervisor{}, Availability: hypervisor.Availability{Available: true}},
			},
			Default: hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		}
	}

	started := time.Now()
	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "probed locally; daemon info unavailable") {
		t.Fatalf("doctor output missing bounded fallback:\n%s", output.String())
	}
	if elapsed := time.Since(started); elapsed > 2*hypervisorProbeTimeout {
		t.Fatalf("doctor took %v with a stalled daemon; the timeout must be paid once, not per finding", elapsed)
	}
	if !strings.Contains(output.String(), "probed locally; daemon info unavailable: i/o timeout") {
		t.Fatalf("doctor output missing stalled-daemon wording:\n%s", output.String())
	}
}

func TestRunDoctorMemoizesDaemonInfo(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	calls := 0
	deps.daemonInfo = func() (daemon.Info, error) {
		calls++
		return daemon.Info{
			BalloonReserveMiB:       4096,
			BalloonDisabled:         true,
			Hypervisors:             []daemon.HypervisorInfo{{Name: "primary", Available: true}},
			DefaultHypervisor:       "primary",
			DefaultHypervisorSource: hypervisor.DefaultSourceCompiled,
		}, nil
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("daemonInfo calls = %d, want 1", calls)
	}
	if !strings.Contains(output.String(), "INFO balloon: daemon started with TBX_DISABLE_BALLOON") {
		t.Fatalf("doctor output missing balloon state from memoized daemon info:\n%s", output.String())
	}
}

func TestRunDoctorFallsBackOnDaemonInfoError(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	deps.daemonInfo = func() (daemon.Info, error) { return daemon.Info{}, errors.New("protocol decode failed") }
	deps.hypervisors = func(context.Context) hypervisor.Registry {
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary": {Hypervisor: doctorTestHypervisor{}, Availability: hypervisor.Availability{Available: true}},
			},
			Default: hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		}
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "probed locally; daemon info unavailable: protocol decode failed") {
		t.Fatalf("doctor output missing fallback for daemon info error:\n%s", output.String())
	}
}

func TestRunDoctorHypervisorProbeIsBounded(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	deps.daemonInfo = func() (daemon.Info, error) {
		return daemon.Info{}, dialError{err: errors.New("connection refused")}
	}
	observedCancellation := make(chan struct{})
	deps.hypervisors = func(ctx context.Context) hypervisor.Registry {
		<-ctx.Done()
		close(observedCancellation)
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary": {Availability: hypervisor.Availability{Reason: ctx.Err().Error()}},
			},
			Default: hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		}
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observedCancellation:
	default:
		t.Fatal("local hypervisor probe did not observe context cancellation")
	}
	if !strings.Contains(output.String(), "availability=unavailable (context deadline exceeded)") {
		t.Fatalf("doctor output missing bounded unavailable result:\n%s", output.String())
	}
}

func TestRunDoctorReportsGuestAgentCapabilityGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		clusters []cluster.Cluster
		listErr  error
		support  hypervisor.FeatureStatus
		want     string
	}{
		{name: "gated host", clusters: []cluster.Cluster{{Name: "demo", TalosExtensions: []string{"qemu-guest-agent"}}}, support: hypervisor.FeatureStatus{Reason: "backend has no guest-agent channel"}, want: "WARN guest-agent: cluster(s) demo request qemu-guest-agent: backend has no guest-agent channel"},
		{name: "capable host", clusters: []cluster.Cluster{{Name: "demo", TalosExtensions: []string{"qemu-guest-agent"}}}, support: hypervisor.FeatureStatus{Supported: true}, want: "PASS guest-agent: channel available for cluster(s) demo"},
		{name: "no cluster requests it", clusters: []cluster.Cluster{{Name: "demo", TalosExtensions: []string{"gvisor"}}}, support: hypervisor.FeatureStatus{Reason: "backend has no guest-agent channel"}, want: "SKIP guest-agent: no cluster requests qemu-guest-agent"},
		{name: "cluster state unreadable", listErr: errors.New("state unreadable"), support: hypervisor.FeatureStatus{Supported: true}, want: "SKIP guest-agent: cluster state unavailable: state unreadable"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			deps := hypervisorDoctorDependencies()
			deps.listConfig = func() ([]cluster.Cluster, error) { return test.clusters, test.listErr }
			deps.daemonInfo = func() (daemon.Info, error) {
				return daemon.Info{
					Hypervisors:             []daemon.HypervisorInfo{{Name: "primary", Available: true, GuestAgent: daemon.NewFeatureStatusInfo(test.support)}},
					DefaultHypervisor:       "primary",
					DefaultHypervisorSource: hypervisor.DefaultSourceCompiled,
				}, nil
			}

			var output strings.Builder
			if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), test.want) {
				t.Fatalf("output = %q, want it to contain %q", output.String(), test.want)
			}
		})
	}
}

func hypervisorDoctorDependencies() doctorDependencies {
	pass := func() error { return nil }
	return doctorDependencies{
		checkHelper:     pass,
		checkResolver:   pass,
		checkDirectDNS:  pass,
		checkForwarding: pass,
		listClusters:    func() ([]daemon.ClusterSummary, error) { return nil, nil },
		listConfig:      func() ([]cluster.Cluster, error) { return nil, nil },
		getStatus:       func() ([]daemon.ClusterStatus, error) { return nil, nil },
		listCache:       func() (daemon.CacheListResult, error) { return daemon.CacheListResult{}, nil },
		hostPressure:    func() (hostpressure.Snapshot, error) { return hostpressure.Snapshot{}, nil },
		command:         func(string, ...string) ([]byte, error) { return nil, nil },
		doHTTP:          func(*http.Request) (*http.Response, error) { return &http.Response{Body: http.NoBody}, nil },
	}
}
