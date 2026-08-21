package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
)

type fakeHypervisor struct {
	architecture hypervisor.Architecture
	capabilities hypervisor.Capabilities
	launch       func(context.Context, hypervisor.Spec) (hypervisor.Machine, error)
	specs        []hypervisor.Spec
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
func (f *fakeMachine) Suspend(context.Context, string) error { return nil }
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
		hypervisor:    backend,
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
		cache:      imagecache.New(root),
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
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
		cache:      imagecache.New(t.TempDir()),
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
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
		hypervisor:    backend,
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
