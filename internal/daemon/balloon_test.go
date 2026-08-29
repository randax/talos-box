package daemon

import (
	"errors"
	"testing"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

func TestBalloonMachineReportsCurrentTarget(t *testing.T) {
	var targeter balloon.CurrentTargeter = balloonMachine{configuredMiB: 4096}
	if got := targeter.CurrentTargetMiB(); got != 4096 {
		t.Fatalf("CurrentTargetMiB() = %d, want configured 4096 before a target is recorded", got)
	}

	targeter = balloonMachine{configuredMiB: 4096, currentTargetMiB: 3072}
	if got := targeter.CurrentTargetMiB(); got != 3072 {
		t.Fatalf("CurrentTargetMiB() = %d, want recorded 3072", got)
	}
}

func TestBalloonablesBypassAPIDProbeWhenBackendReportsBalloonReadback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("balloon-qemu", 0, 1, 0, cluster.NodeDefaults{MemoryMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}

	machine := &fakeMachine{active: true, setMemoryErr: hypervisor.ErrDeviceNotActive}
	service := &Server{
		hypervisor: &fakeHypervisor{
			capabilities: hypervisor.Capabilities{
				BalloonReadback: hypervisor.FeatureStatus{Supported: true},
			},
		},
		vms: map[string]map[string]hypervisor.Machine{
			item.Name: {item.Nodes[0].Name: machine},
		},
	}

	vms := service.Balloonables()
	if len(vms) != 1 {
		t.Fatalf("Balloonables() count = %d, want 1 running node without apid gating", len(vms))
	}
	if err := vms[item.Name+"/"+item.Nodes[0].Name].SetMemoryTargetMiB(1024); err != nil {
		t.Fatalf("SetMemoryTargetMiB() = %v, want ErrDeviceNotActive tolerated on QEMU path", err)
	}
}

func TestBalloonablesKeepAPIDGateWithoutBalloonReadback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("balloon-vz", 0, 1, 0, cluster.NodeDefaults{MemoryMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}

	service := &Server{
		hypervisor: &fakeHypervisor{
			capabilities: hypervisor.Capabilities{
				BalloonReadback: hypervisor.FeatureStatus{
					Reason: "Virtualization.framework does not report the guest-visible balloon size",
				},
			},
		},
		vms: map[string]map[string]hypervisor.Machine{
			item.Name: {item.Nodes[0].Name: &fakeMachine{active: true}},
		},
	}

	if got := service.Balloonables(); len(got) != 0 {
		t.Fatalf("Balloonables() = %d entries, want apid gate to exclude unreachable VZ guest", len(got))
	}
}

func TestBalloonMachineOnlyToleratesInactiveDeviceOnCapabilityDrivenPath(t *testing.T) {
	errDeviceInactive := hypervisor.ErrDeviceNotActive

	if err := (balloonMachine{
		machine:                 &fakeMachine{setMemoryErr: errDeviceInactive},
		tolerateDeviceNotActive: true,
	}).SetMemoryTargetMiB(1024); err != nil {
		t.Fatalf("SetMemoryTargetMiB() = %v, want nil when inactive devices are tolerated", err)
	}

	err := (balloonMachine{
		machine: &fakeMachine{setMemoryErr: errDeviceInactive},
	}).SetMemoryTargetMiB(1024)
	if !errors.Is(err, errDeviceInactive) {
		t.Fatalf("SetMemoryTargetMiB() = %v, want ErrDeviceNotActive when capability path is not enabled", err)
	}
}
