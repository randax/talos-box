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
					Name: hypervisor.NameVZ, Available: true,
					BalloonReadback: daemon.FeatureStatusInfo{Supported: true},
					GuestAgent:      daemon.FeatureStatusInfo{Reason: "no channel"},
				},
				{Name: hypervisor.NameQEMU, AvailabilityReason: "optional probe failed", AvailabilityRemediation: "install optional support"},
			},
			DefaultHypervisor:       hypervisor.NameVZ,
			DefaultHypervisorSource: hypervisor.DefaultSourceCompiled,
		}, nil
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	wantOptional := "INFO Hypervisors: qemu: availability=unavailable (optional probe failed; remediation: install optional support); default=no; balloon-readback=unavailable; suspend=unavailable; suspend-survives-restart=unavailable; guest-agent=unavailable"
	wantPrimary := "INFO Hypervisors: vz: availability=available; default=yes (source=compiled); balloon-readback=supported; suspend=unsupported; suspend-survives-restart=unsupported; guest-agent=unsupported (no channel)"
	text := output.String()
	optionalIndex, primaryIndex := strings.Index(text, wantOptional), strings.Index(text, wantPrimary)
	if optionalIndex < 0 || primaryIndex < 0 {
		t.Fatalf("doctor output missing hypervisor lines:\n%s", text)
	}
	if optionalIndex > primaryIndex {
		t.Fatalf("hypervisor lines are not lexically sorted:\n%s", text)
	}
}

func TestRunDoctorShowsEnvironmentHypervisorDefaultSource(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	deps.daemonInfo = func() (daemon.Info, error) {
		return daemon.Info{
			Hypervisors:             []daemon.HypervisorInfo{{Name: "selected", Available: true}},
			DefaultHypervisor:       "selected",
			DefaultHypervisorSource: hypervisor.DefaultSourceEnvironment,
		}, nil
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "default=yes (source=TBX_HYPERVISOR)") {
		t.Fatalf("doctor output missing environment default source:\n%s", output.String())
	}
}

func TestRunDoctorReportsHypervisorsWithDaemonDown(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	deps.daemonInfo = func() (daemon.Info, error) {
		return daemon.Info{}, dialError{err: errors.New("connection refused")}
	}
	deps.hypervisors = func(context.Context) (hypervisor.Registry, error) {
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary": {
					Hypervisor: doctorTestHypervisor{capabilities: hypervisor.Capabilities{
						Suspend:    hypervisor.FeatureStatus{Supported: true},
						GuestAgent: hypervisor.FeatureStatus{Supported: true},
					}},
					Availability: hypervisor.Availability{Available: true},
				},
			},
			Default: hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		}, nil
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
		"suspend=supported; suspend-survives-restart=unsupported",
		"guest-agent=supported",
		"PASS guest-agent: channel available for cluster(s) demo",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, output.String())
		}
	}
}

func TestHypervisorFindingsReportSuspendSeparatelyFromRestartSurvival(t *testing.T) {
	t.Parallel()
	findings := hypervisorFindings(doctorHypervisorInventory{
		items: []daemon.HypervisorInfo{
			{
				Name:                         hypervisor.NameQEMU,
				Available:                    true,
				Suspend:                      daemon.FeatureStatusInfo{Supported: true},
				SuspendSurvivesDaemonRestart: true,
			},
			{
				Name:      hypervisor.NameVZ,
				Available: true,
				Suspend:   daemon.FeatureStatusInfo{Supported: true},
			},
		},
	})

	if got := findings[0].detail; !strings.Contains(got, "suspend=supported; suspend-survives-restart=supported") {
		t.Fatalf("QEMU finding = %q, want both suspend gates supported", got)
	}
	if got := findings[1].detail; !strings.Contains(got, "suspend=supported; suspend-survives-restart=unsupported") {
		t.Fatalf("VZ finding = %q, want suspend supported and restart survival unsupported", got)
	}
}

func TestHypervisorFindingsBestEffortPlatformTag(t *testing.T) {
	originalGOOS, originalGOARCH := doctorGOOS, doctorGOARCH
	t.Cleanup(func() {
		doctorGOOS, doctorGOARCH = originalGOOS, originalGOARCH
	})

	tests := []struct {
		name         string
		goos         string
		goarch       string
		backend      daemon.HypervisorInfo
		availability string
	}{
		{
			name: "darwin amd64 qemu", goos: "darwin", goarch: "amd64",
			backend:      daemon.HypervisorInfo{Name: hypervisor.NameQEMU, Available: true},
			availability: "availability=available (best-effort platform)",
		},
		{
			name: "darwin arm64 qemu", goos: "darwin", goarch: "arm64",
			backend:      daemon.HypervisorInfo{Name: hypervisor.NameQEMU, Available: true},
			availability: "availability=available;",
		},
		{
			name: "darwin amd64 vz", goos: "darwin", goarch: "amd64",
			backend:      daemon.HypervisorInfo{Name: hypervisor.NameVZ, Available: true},
			availability: "availability=available;",
		},
		{
			name: "linux amd64 qemu", goos: "linux", goarch: "amd64",
			backend:      daemon.HypervisorInfo{Name: hypervisor.NameQEMU, Available: true},
			availability: "availability=available;",
		},
		{
			name: "unavailable darwin amd64 qemu", goos: "darwin", goarch: "amd64",
			backend: daemon.HypervisorInfo{
				Name: hypervisor.NameQEMU, AvailabilityReason: "HVF unavailable", AvailabilityRemediation: "enable virtualization",
			},
			availability: "availability=unavailable (HVF unavailable; remediation: enable virtualization)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doctorGOOS, doctorGOARCH = test.goos, test.goarch
			findings := hypervisorFindings(doctorHypervisorInventory{items: []daemon.HypervisorInfo{test.backend}})
			if got := findings[0].detail; !strings.Contains(got, test.availability) {
				t.Fatalf("finding = %q, want %q", got, test.availability)
			}
		})
	}
}

func TestRunDoctorLocalFallbackUsesEnvironmentHypervisorDefault(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	deps.daemonInfo = func() (daemon.Info, error) {
		return daemon.Info{}, dialError{err: errors.New("connection refused")}
	}
	deps.hypervisors = func(context.Context) (hypervisor.Registry, error) {
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				hypervisor.NameVZ:   {Hypervisor: doctorTestHypervisor{}, Availability: hypervisor.Availability{Available: true}},
				hypervisor.NameQEMU: {Hypervisor: doctorTestHypervisor{}, Availability: hypervisor.Availability{Available: true}},
			},
			Default:         hypervisor.Default{Name: hypervisor.NameQEMU, Source: hypervisor.DefaultSourceEnvironment},
			CompiledDefault: hypervisor.NameVZ,
		}, nil
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "default=yes (source=TBX_HYPERVISOR)") {
		t.Fatalf("doctor output missing environment-selected local default:\n%s", output.String())
	}
}

func TestRunDoctorLocalFallbackReportsInvalidEnvironment(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	deps.daemonInfo = func() (daemon.Info, error) {
		return daemon.Info{}, dialError{err: errors.New("connection refused")}
	}
	deps.hypervisors = func(context.Context) (hypervisor.Registry, error) {
		return hypervisor.Registry{}, errors.New(`TBX_HYPERVISOR: hypervisor must be one of vz | qemu (got "xen")`)
	}

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatal(err)
	}
	want := `INFO Hypervisors: inventory unavailable: TBX_HYPERVISOR: hypervisor must be one of vz | qemu (got "xen")`
	if !strings.Contains(output.String(), want) {
		t.Fatalf("doctor output missing invalid environment error:\n%s", output.String())
	}
}

func TestRunDoctorFallsBackWhenDaemonReportsNoHypervisorInventory(t *testing.T) {
	t.Parallel()
	deps := hypervisorDoctorDependencies()
	deps.daemonInfo = func() (daemon.Info, error) { return daemon.Info{}, nil }
	deps.hypervisors = func(context.Context) (hypervisor.Registry, error) {
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary": {
					Hypervisor:   doctorTestHypervisor{capabilities: hypervisor.Capabilities{GuestAgent: hypervisor.FeatureStatus{Supported: true}}},
					Availability: hypervisor.Availability{Available: true},
				},
			},
			Default: hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		}, nil
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
	deps.hypervisors = func(context.Context) (hypervisor.Registry, error) {
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary": {Hypervisor: doctorTestHypervisor{}, Availability: hypervisor.Availability{Available: true}},
			},
			Default: hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		}, nil
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
	deps.hypervisors = func(context.Context) (hypervisor.Registry, error) {
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary": {Hypervisor: doctorTestHypervisor{}, Availability: hypervisor.Availability{Available: true}},
			},
			Default: hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		}, nil
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
	deps.hypervisors = func(ctx context.Context) (hypervisor.Registry, error) {
		<-ctx.Done()
		close(observedCancellation)
		return hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary": {Availability: hypervisor.Availability{Reason: ctx.Err().Error()}},
			},
			Default: hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		}, nil
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
		statuses []daemon.ClusterStatus
		listErr  error
		info     daemon.Info
		want     string
	}{
		{
			name:     "unsupported cluster",
			clusters: []cluster.Cluster{{Name: "demo", Hypervisor: string(hypervisor.NameVZ), TalosExtensions: []string{"qemu-guest-agent"}}},
			info: daemon.Info{
				Hypervisors: []daemon.HypervisorInfo{
					{Name: hypervisor.NameVZ, Available: true, GuestAgent: daemon.FeatureStatusInfo{Reason: "backend has no guest-agent channel"}},
					{Name: hypervisor.NameQEMU, Available: true, GuestAgent: daemon.FeatureStatusInfo{Supported: true}},
				},
				DefaultHypervisor:       hypervisor.NameQEMU,
				DefaultHypervisorSource: hypervisor.DefaultSourceEnvironment,
			},
			want: "WARN guest-agent: cluster(s) demo request qemu-guest-agent: backend has no guest-agent channel",
		},
		{
			name:     "supported cluster",
			clusters: []cluster.Cluster{{Name: "demo", Hypervisor: string(hypervisor.NameQEMU), TalosExtensions: []string{"qemu-guest-agent"}}},
			info: daemon.Info{
				Hypervisors:             []daemon.HypervisorInfo{{Name: hypervisor.NameQEMU, Available: true, GuestAgent: daemon.FeatureStatusInfo{Supported: true}}},
				DefaultHypervisor:       hypervisor.NameVZ,
				DefaultHypervisorSource: hypervisor.DefaultSourceCompiled,
			},
			want: "PASS guest-agent: channel available for cluster(s) demo",
		},
		{
			name: "mixed supported and unsupported clusters",
			clusters: []cluster.Cluster{
				{Name: "qemu-demo", Hypervisor: string(hypervisor.NameQEMU), TalosExtensions: []string{"qemu-guest-agent"}},
				{Name: "vz-demo", Hypervisor: string(hypervisor.NameVZ), TalosExtensions: []string{"qemu-guest-agent"}},
			},
			statuses: []daemon.ClusterStatus{
				{Name: "qemu-demo", Hypervisor: hypervisor.NameQEMU, Capabilities: []daemon.CapabilityStatus{{Name: "qemu-guest-agent", Supported: true}}},
				{Name: "vz-demo", Hypervisor: hypervisor.NameVZ, Capabilities: []daemon.CapabilityStatus{{Name: "qemu-guest-agent", Reason: "backend has no guest-agent channel"}}},
			},
			info: daemon.Info{
				Hypervisors: []daemon.HypervisorInfo{
					{Name: hypervisor.NameVZ, Available: true, GuestAgent: daemon.FeatureStatusInfo{Reason: "backend has no guest-agent channel"}},
					{Name: hypervisor.NameQEMU, Available: true, GuestAgent: daemon.FeatureStatusInfo{Supported: true}},
				},
				DefaultHypervisor:       hypervisor.NameVZ,
				DefaultHypervisorSource: hypervisor.DefaultSourceCompiled,
			},
			want: "WARN guest-agent: channel available for cluster(s) qemu-demo; cluster(s) vz-demo request qemu-guest-agent: backend has no guest-agent channel",
		},
		{
			name:     "legacy empty state uses compiled default",
			clusters: []cluster.Cluster{{Name: "legacy", TalosExtensions: []string{"qemu-guest-agent"}}},
			info: daemon.Info{
				Hypervisors: []daemon.HypervisorInfo{
					{Name: hypervisor.NameVZ, Available: true, GuestAgent: daemon.FeatureStatusInfo{Reason: "backend has no guest-agent channel"}},
					{Name: hypervisor.NameQEMU, Available: true, GuestAgent: daemon.FeatureStatusInfo{Supported: true}},
				},
				DefaultHypervisor:         hypervisor.NameQEMU,
				DefaultHypervisorSource:   hypervisor.DefaultSourceEnvironment,
				CompiledDefaultHypervisor: hypervisor.NameVZ,
			},
			want: "WARN guest-agent: cluster(s) legacy request qemu-guest-agent: backend has no guest-agent channel",
		},
		{
			name:     "no cluster requests it",
			clusters: []cluster.Cluster{{Name: "demo", TalosExtensions: []string{"gvisor"}}},
			info: daemon.Info{
				Hypervisors:             []daemon.HypervisorInfo{{Name: "primary", Available: true, GuestAgent: daemon.FeatureStatusInfo{Reason: "backend has no guest-agent channel"}}},
				DefaultHypervisor:       "primary",
				DefaultHypervisorSource: hypervisor.DefaultSourceCompiled,
			},
			want: "SKIP guest-agent: no cluster requests qemu-guest-agent",
		},
		{
			name:    "cluster state unreadable",
			listErr: errors.New("state unreadable"),
			info: daemon.Info{
				Hypervisors:             []daemon.HypervisorInfo{{Name: "primary", Available: true, GuestAgent: daemon.FeatureStatusInfo{Supported: true}}},
				DefaultHypervisor:       "primary",
				DefaultHypervisorSource: hypervisor.DefaultSourceCompiled,
			},
			want: "SKIP guest-agent: cluster state unavailable: state unreadable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			deps := hypervisorDoctorDependencies()
			deps.listConfig = func() ([]cluster.Cluster, error) { return test.clusters, test.listErr }
			deps.getStatus = func() ([]daemon.ClusterStatus, error) { return test.statuses, nil }
			deps.daemonInfo = func() (daemon.Info, error) { return test.info, nil }

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
