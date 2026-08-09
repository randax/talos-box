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
)

type fakeHypervisor struct {
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

type fakeMachine struct {
	active        bool
	calls         []string
	stopDeadline  bool
	stopRemaining time.Duration
	stopErr       error
	closeErr      error
}

func (f *fakeMachine) Active() bool                 { return f.active }
func (f *fakeMachine) SetMemoryTargetMiB(int) error { return nil }
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
