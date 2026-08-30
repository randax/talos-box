package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
)

type fakeHypervisor struct {
	architecture hypervisor.Architecture
	capabilities hypervisor.Capabilities
	launch       func(context.Context, hypervisor.Spec) (hypervisor.Machine, error)
	specs        []hypervisor.Spec
}

func fakeRegistry(defaultName hypervisor.Name, backends map[hypervisor.Name]hypervisor.Hypervisor) hypervisor.Registry {
	registered := make(map[hypervisor.Name]hypervisor.Backend, len(backends))
	for name, backend := range backends {
		registered[name] = hypervisor.Backend{
			Hypervisor:   backend,
			Availability: hypervisor.Availability{Available: true},
		}
	}
	return hypervisor.Registry{
		Backends:        registered,
		Default:         hypervisor.Default{Name: defaultName, Source: hypervisor.DefaultSourceCompiled},
		CompiledDefault: defaultName,
	}
}

func singleFakeRegistry(backend hypervisor.Hypervisor) hypervisor.Registry {
	return fakeRegistry("fake", map[hypervisor.Name]hypervisor.Hypervisor{"fake": backend})
}

func setFakeHypervisor(server *Server, backend *fakeHypervisor) {
	server.hypervisors = singleFakeRegistry(backend)
}

func defaultFakeHypervisor(server *Server) *fakeHypervisor {
	_, backend, err := server.hypervisors.ResolveDefault()
	if err != nil {
		panic(err)
	}
	return backend.(*fakeHypervisor)
}

func TestNewServerWithRegistryAllowsUnavailableNonDefaultHypervisor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	registry := singleFakeRegistry(&fakeHypervisor{})
	registry.Backends["optional"] = hypervisor.Backend{
		Availability: hypervisor.Availability{Reason: "optional probe failed"},
	}

	server, err := newServer(context.Background(), registry)
	if err != nil {
		t.Fatalf("newServer() = %v, want unavailable optional backend to be retained", err)
	}
	t.Cleanup(func() { _ = server.Shutdown() })
	if got := server.hypervisors.Backends["optional"].Availability.Reason; got != "optional probe failed" {
		t.Fatalf("retained optional reason = %q", got)
	}
}

func TestNewServerWithRegistryAllowsUnavailableDefaultHypervisor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	registry := hypervisor.Registry{
		Backends: map[hypervisor.Name]hypervisor.Backend{
			"primary": {Availability: hypervisor.Availability{Reason: "default probe failed"}},
		},
		Default:         hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
		CompiledDefault: "primary",
	}

	server, err := newServer(context.Background(), registry)
	if err != nil {
		t.Fatalf("newServer() = %v, want unavailable default backend to be retained", err)
	}
	t.Cleanup(func() { _ = server.Shutdown() })
}

func TestHypervisorForClusterUsesPersistedSelection(t *testing.T) {
	t.Parallel()
	persisted := &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64}
	selectedDefault := &fakeHypervisor{architecture: hypervisor.ArchitectureARM64}
	registry := fakeRegistry("selected", map[hypervisor.Name]hypervisor.Hypervisor{
		"persisted": persisted,
		"selected":  selectedDefault,
	})
	server := &Server{hypervisors: registry}

	name, backend, err := server.hypervisorForCluster(cluster.Cluster{Name: "ordinary", Hypervisor: "persisted"})
	if err != nil {
		t.Fatal(err)
	}
	if name != "persisted" || backend != persisted {
		t.Fatalf("hypervisorForCluster() = (%q, %p), want persisted backend %p", name, backend, persisted)
	}
}

func TestHypervisorForClusterUsesCompiledDefaultForLegacyState(t *testing.T) {
	t.Parallel()
	compiled := &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64}
	effective := &fakeHypervisor{architecture: hypervisor.ArchitectureARM64}
	registry := fakeRegistry("effective", map[hypervisor.Name]hypervisor.Hypervisor{
		"compiled":  compiled,
		"effective": effective,
	})
	registry.CompiledDefault = "compiled"
	server := &Server{hypervisors: registry}

	name, backend, err := server.hypervisorForCluster(cluster.Cluster{Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if name != "compiled" || backend != compiled {
		t.Fatalf("hypervisorForCluster() = (%q, %p), want compiled backend %p", name, backend, compiled)
	}
}

func TestHypervisorForCreateUsesExplicitThenEffectiveDefault(t *testing.T) {
	t.Parallel()
	explicit := &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64}
	effective := &fakeHypervisor{architecture: hypervisor.ArchitectureARM64}
	registry := fakeRegistry("effective", map[hypervisor.Name]hypervisor.Hypervisor{
		"explicit":  explicit,
		"effective": effective,
	})
	server := &Server{hypervisors: registry}

	name, backend, err := server.hypervisorForCreate("explicit")
	if err != nil {
		t.Fatal(err)
	}
	if name != "explicit" || backend != explicit {
		t.Fatalf("hypervisorForCreate(explicit) = (%q, %p), want explicit backend %p", name, backend, explicit)
	}

	name, backend, err = server.hypervisorForCreate("")
	if err != nil {
		t.Fatal(err)
	}
	if name != "effective" || backend != effective {
		t.Fatalf("hypervisorForCreate(empty) = (%q, %p), want effective default %p", name, backend, effective)
	}
}

func TestCreateAgainstUnavailableHypervisorReturnsGateReasonBeforeHostProbes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hostProbed := false
	service := &Server{
		hypervisors: hypervisor.Registry{
			Backends: map[hypervisor.Name]hypervisor.Backend{
				"primary":  {Hypervisor: &fakeHypervisor{}, Availability: hypervisor.Availability{Available: true}},
				"optional": {Availability: hypervisor.Availability{Reason: "optional capability is unavailable"}},
			},
			Default:         hypervisor.Default{Name: "primary", Source: hypervisor.DefaultSourceCompiled},
			CompiledDefault: "primary",
		},
		hostPressure: func(string) (hostpressure.Snapshot, error) {
			hostProbed = true
			return hostpressure.Snapshot{}, nil
		},
	}
	raw, err := json.Marshal(createArgs{Name: "gated", Hypervisor: "optional"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.createCluster(raw, nil)
	if err == nil || !strings.Contains(err.Error(), "optional capability is unavailable") {
		t.Fatalf("createCluster() error = %v, want hypervisor availability reason", err)
	}
	if hostProbed {
		t.Fatal("createCluster() probed host pressure before rejecting the unavailable hypervisor")
	}
	if _, loadErr := cluster.Load("gated"); !errors.Is(loadErr, os.ErrNotExist) {
		t.Fatalf("cluster.Load(gated) error = %v, want no persisted state", loadErr)
	}
}

func TestCreateRecordsResolvedHypervisorAndArchitecture(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	const schematic, version = "selected-schematic", "v1.13.6"
	disk := filepath.Join(root, schematic, version, string(hypervisor.ArchitectureAMD64), "disk.raw")
	if err := os.MkdirAll(filepath.Dir(disk), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(disk, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected := &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64}
	service := &Server{
		cache:         imagecache.New(root),
		hypervisors:   fakeRegistry("selected", map[hypervisor.Name]hypervisor.Hypervisor{"selected": selected}),
		vms:           make(map[string]map[string]hypervisor.Machine),
		helperCheck:   func() error { return nil },
		hostPressure:  noHostPressure,
		subnetSources: emptySubnetSources(),
	}
	zero := 0
	raw, err := json.Marshal(createArgs{
		Name: "recorded", ControlPlanes: &zero, Workers: &zero,
		Hypervisor: "selected", Schematic: schematic, Version: version,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.createCluster(raw, nil); err != nil {
		t.Fatal(err)
	}
	item, err := cluster.Load("recorded")
	if err != nil {
		t.Fatal(err)
	}
	if item.Hypervisor != "selected" || item.ImageArchitecture != string(hypervisor.ArchitectureAMD64) {
		t.Fatalf("stored selection = (%q, %q), want selected hypervisor and architecture", item.Hypervisor, item.ImageArchitecture)
	}
}

func TestStatusReportsEffectiveHypervisorForLegacyAndNewState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, item := range []cluster.Cluster{
		{Name: "legacy", SubnetIndex: 1},
		{Name: "persisted", SubnetIndex: 2, Hypervisor: "selected"},
	} {
		if err := cluster.Save(item); err != nil {
			t.Fatal(err)
		}
	}
	registry := fakeRegistry("selected", map[hypervisor.Name]hypervisor.Hypervisor{
		"compiled": &fakeHypervisor{},
		"selected": &fakeHypervisor{},
	})
	registry.CompiledDefault = "compiled"
	service := &Server{hypervisors: registry, vms: make(map[string]map[string]hypervisor.Machine)}

	statuses, err := service.status(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]hypervisor.Name, len(statuses))
	for _, status := range statuses {
		got[status.Name] = status.Hypervisor
	}
	if got["legacy"] != "compiled" || got["persisted"] != "selected" {
		t.Fatalf("status hypervisors = %v, want legacy compiled and persisted selected", got)
	}
}

func TestInfoReportsHypervisorAvailabilityRemediation(t *testing.T) {
	t.Parallel()
	service := &Server{hypervisors: hypervisor.Registry{
		Backends: map[hypervisor.Name]hypervisor.Backend{
			hypervisor.NameQEMU: {Availability: hypervisor.Availability{
				Reason: "runtime unavailable", Remediation: "install runtime support",
			}},
		},
		Default:         hypervisor.Default{Name: hypervisor.NameQEMU, Source: hypervisor.DefaultSourceEnvironment},
		CompiledDefault: hypervisor.NameVZ,
	}}

	info := service.info()
	if len(info.Hypervisors) != 1 || info.Hypervisors[0].AvailabilityRemediation != "install runtime support" {
		t.Fatalf("info hypervisors = %+v, want availability remediation", info.Hypervisors)
	}
}

func TestInfoReportsHypervisorSuspendCapability(t *testing.T) {
	t.Parallel()
	service := &Server{hypervisors: fakeRegistry(hypervisor.NameQEMU, map[hypervisor.Name]hypervisor.Hypervisor{
		hypervisor.NameQEMU: &fakeHypervisor{capabilities: hypervisor.Capabilities{
			Suspend:                      hypervisor.FeatureStatus{Supported: true},
			SuspendSurvivesDaemonRestart: true,
		}},
	})}

	info := service.info()
	if len(info.Hypervisors) != 1 {
		t.Fatalf("info hypervisors = %+v, want exactly one backend", info.Hypervisors)
	}
	suspend := info.Hypervisors[0].Suspend
	if suspend == nil {
		t.Fatalf("info hypervisors = %+v, want the suspend gate populated for an available backend", info.Hypervisors)
	}
	if !suspend.Supported || !info.Hypervisors[0].SuspendSurvivesDaemonRestart {
		t.Fatalf("info hypervisors = %+v, want suspend and restart-survival capabilities", info.Hypervisors)
	}
}

func TestBalloonCandidatesUseResolvedHypervisorCapabilities(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	items := make([]cluster.Cluster, 0, 2)
	for index, selection := range []string{"readback", "conservative"} {
		item, err := cluster.New(selection, index, 1, 0, cluster.NodeDefaults{MemoryMiB: 2048})
		if err != nil {
			t.Fatal(err)
		}
		item.Hypervisor = selection
		if err := cluster.Save(item); err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	server := &Server{
		hypervisors: fakeRegistry("readback", map[hypervisor.Name]hypervisor.Hypervisor{
			"readback": &fakeHypervisor{capabilities: hypervisor.Capabilities{
				BalloonReadback: hypervisor.FeatureStatus{Supported: true},
			}},
			"conservative": &fakeHypervisor{},
		}),
		vms: map[string]map[string]hypervisor.Machine{
			items[0].Name: {items[0].Nodes[0].Name: &fakeMachine{active: true}},
			items[1].Name: {items[1].Nodes[0].Name: &fakeMachine{active: true}},
		},
	}
	candidates := server.balloonCandidatesLocked()
	readbackKey := items[0].Name + "/" + items[0].Nodes[0].Name
	conservativeKey := items[1].Name + "/" + items[1].Nodes[0].Name
	if !candidates[readbackKey].balloonReadback {
		t.Fatalf("candidate %+v does not carry resolved balloon readback", candidates[readbackKey])
	}
	if candidates[conservativeKey].balloonReadback {
		t.Fatalf("candidate %+v unexpectedly carries balloon readback", candidates[conservativeKey])
	}
	candidate := candidates[conservativeKey]
	candidate.ip = ""
	if got := balloonablesFrom(map[string]balloonCandidate{conservativeKey: candidate}, nil, func() bool { return false }); len(got) != 0 {
		t.Fatalf("balloonablesFrom() = %v, want conservative eligibility without readback", got)
	}
}

func (f *fakeHypervisor) Launch(ctx context.Context, spec hypervisor.Spec) (hypervisor.Machine, error) {
	f.specs = append(f.specs, spec)
	if f.launch != nil {
		return f.launch(ctx, spec)
	}
	return &fakeMachine{active: true}, nil
}

func (f *fakeHypervisor) Capabilities() hypervisor.Capabilities { return f.capabilities }

func (f *fakeHypervisor) Architecture() hypervisor.Architecture {
	if f.architecture == "" {
		return hypervisor.ArchitectureARM64
	}
	return f.architecture
}

type fakeMachine struct {
	active        bool
	setMemoryErr  error
	memoryTargets []int
	calls         []string
	stopDeadline  bool
	stopRemaining time.Duration
	stopErr       error
	closeErr      error
	onClose       func()
	onSuspend     func(savePath string) error
}

func (f *fakeMachine) Active() bool { return f.active }
func (f *fakeMachine) SetMemoryTargetMiB(targetMiB int) error {
	if f.setMemoryErr != nil {
		return f.setMemoryErr
	}
	f.memoryTargets = append(f.memoryTargets, targetMiB)
	return nil
}
func (f *fakeMachine) Stop(ctx context.Context) error {
	f.calls = append(f.calls, "stop")
	if deadline, ok := ctx.Deadline(); ok {
		f.stopDeadline = true
		f.stopRemaining = time.Until(deadline)
	}
	f.active = false
	return f.stopErr
}
func (f *fakeMachine) Suspend(_ context.Context, savePath string) error {
	if f.onSuspend != nil {
		return f.onSuspend(savePath)
	}
	return nil
}
func (f *fakeMachine) Close() error {
	f.calls = append(f.calls, "close")
	if f.onClose != nil {
		f.onClose()
	}
	return f.closeErr
}

func TestStartLaunchesMachinesThroughInjectedHypervisor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("fake-launch", 0, 1, 0, cluster.NodeDefaults{CPUs: 2, MemoryMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeHypervisor{}
	service := &Server{
		hypervisors:   singleFakeRegistry(backend),
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: emptySubnetSources(),
	}
	if _, err := service.start(item); err != nil {
		t.Fatal(err)
	}
	if len(backend.specs) != 1 {
		t.Fatalf("Launch calls = %d, want 1", len(backend.specs))
	}
	spec := backend.specs[0]
	if spec.CPUs != 2 || spec.MemoryMiB != 2048 || spec.MAC != item.Nodes[0].MAC {
		t.Fatalf("Launch spec = %+v, want cluster node sizing and MAC", spec)
	}
	if !service.nodeRunning(item.Name, item.Nodes[0].Name) {
		t.Fatal("launched fake machine is not tracked as running")
	}
}

func TestCachedDiskUsesHypervisorArchitecture(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for architecture, contents := range map[hypervisor.Architecture]string{
		hypervisor.ArchitectureAMD64: "amd64 disk",
		hypervisor.ArchitectureARM64: "arm64 disk",
	} {
		path := filepath.Join(root, "test-schematic", "v1.2.3", string(architecture), "disk.raw")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	service := &Server{
		cache:       imagecache.New(root),
		hypervisors: singleFakeRegistry(&fakeHypervisor{architecture: hypervisor.ArchitectureAMD64}),
	}
	item := cluster.Cluster{Schematic: "test-schematic", TalosVersion: "v1.2.3", ImageArchitecture: "amd64"}
	path, err := service.cachedDisk(item)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "test-schematic", "v1.2.3", "amd64", "disk.raw")
	if path != want {
		t.Fatalf("cachedDisk() = %q, want target hypervisor path %q", path, want)
	}
}

func TestCachedDiskRejectsClusterHypervisorArchitectureMismatch(t *testing.T) {
	t.Parallel()

	service := &Server{
		cache:       imagecache.New(t.TempDir()),
		hypervisors: singleFakeRegistry(&fakeHypervisor{architecture: hypervisor.ArchitectureAMD64}),
	}
	item := cluster.Cluster{
		Name:              "arm-cluster",
		Schematic:         "test-schematic",
		TalosVersion:      "v1.2.3",
		ImageArchitecture: "arm64",
	}
	_, err := service.cachedDisk(item)
	if err == nil || !strings.Contains(err.Error(), "cluster \"arm-cluster\" uses arm64 images, but the active hypervisor targets amd64") {
		t.Fatalf("cachedDisk() error = %v, want architecture mismatch", err)
	}
}

func TestCloseMachineSuppliesStopDeadlineAndAlwaysCloses(t *testing.T) {
	stopErr := errors.New("stop failed")
	closeErr := errors.New("close failed")
	machine := &fakeMachine{active: true, stopErr: stopErr, closeErr: closeErr}

	err := closeMachine(machine)
	if !errors.Is(err, stopErr) || !errors.Is(err, closeErr) {
		t.Fatalf("closeMachine() = %v, want joined stop and close errors", err)
	}
	if !machine.stopDeadline {
		t.Fatal("daemon did not supply a stop deadline")
	}
	if machine.stopRemaining < 29*time.Second || machine.stopRemaining > machineStopTimeout {
		t.Fatalf("stop deadline remaining = %v, want approximately %v", machine.stopRemaining, machineStopTimeout)
	}
	if got, want := machine.calls, []string{"stop", "close"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestResumeUsesLaunchRestoreAndReportsColdBootFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("fake-resume", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	savePath := filepath.Join(dir, item.Nodes[0].Name+".vzstate")
	if err := os.WriteFile(savePath, []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}

	backend := &fakeHypervisor{launch: func(_ context.Context, spec hypervisor.Spec) (hypervisor.Machine, error) {
		if spec.Restore == nil {
			t.Fatal("resume Launch spec has no restore field")
		}
		spec.Restore.Fallback(hypervisor.ErrIncompatibleSave)
		return &fakeMachine{active: true}, nil
	}}
	service := &Server{
		hypervisors:   singleFakeRegistry(backend),
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: emptySubnetSources(),
	}
	raw := []byte(`{"name":"fake-resume"}`)
	result, err := service.resumeCluster(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Warning, "saved state could not be restored; cold-booting instead") {
		t.Fatalf("resume warning = %q, want cold-boot fallback", result.Warning)
	}
	if len(backend.specs) != 1 || backend.specs[0].Restore.Path != savePath {
		t.Fatalf("restore launch specs = %+v, want path %q", backend.specs, savePath)
	}
	if _, err := os.Stat(savePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful fallback did not consume saved state: %v", err)
	}
}

func emptySubnetSources() cluster.SubnetSources {
	return cluster.SubnetSources{
		Interfaces: func() ([]cluster.HostInterface, error) { return nil, nil },
		Route:      func(net.IP) (cluster.HostRoute, error) { return cluster.HostRoute{}, nil },
	}
}
