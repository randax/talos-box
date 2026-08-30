package daemon

import (
	"errors"
	"strings"
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
		hypervisors: singleFakeRegistry(&fakeHypervisor{
			capabilities: hypervisor.Capabilities{
				BalloonReadback: hypervisor.FeatureStatus{Supported: true},
			},
		}),
		vms: map[string]map[string]hypervisor.Machine{
			item.Name: {item.Nodes[0].Name: machine},
		},
	}

	vms := service.Balloonables()
	if len(vms) != 1 {
		t.Fatalf("Balloonables() count = %d, want 1 running node without apid gating", len(vms))
	}
	if err := vms[item.Name+"/"+item.Nodes[0].Name].SetMemoryTargetMiB(1024); !errors.Is(err, balloon.ErrTargetPending) {
		t.Fatalf("SetMemoryTargetMiB() = %v, want ErrTargetPending on QEMU path", err)
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
		hypervisors: singleFakeRegistry(&fakeHypervisor{
			capabilities: hypervisor.Capabilities{
				BalloonReadback: hypervisor.FeatureStatus{
					Reason: "Virtualization.framework does not report the guest-visible balloon size",
				},
			},
		}),
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
	recorded := 0

	err := (balloonMachine{
		machine:                 &fakeMachine{setMemoryErr: errDeviceInactive},
		tolerateDeviceNotActive: true,
		recordTarget:            func(int) { recorded++ },
	}).SetMemoryTargetMiB(1024)
	if !errors.Is(err, balloon.ErrTargetPending) {
		t.Fatalf("SetMemoryTargetMiB() = %v, want ErrTargetPending when inactive devices are tolerated", err)
	}
	if recorded != 0 {
		t.Fatalf("recordTarget calls=%d, want none for a target the guest did not accept", recorded)
	}

	err = (balloonMachine{
		machine: &fakeMachine{setMemoryErr: errDeviceInactive},
	}).SetMemoryTargetMiB(1024)
	if !errors.Is(err, errDeviceInactive) {
		t.Fatalf("SetMemoryTargetMiB() = %v, want ErrDeviceNotActive when capability path is not enabled", err)
	}
}

func TestBalloonReclaimDoesNotCreditAPendingTarget(t *testing.T) {
	pending := balloonMachine{
		machine:                 &fakeMachine{setMemoryErr: hypervisor.ErrDeviceNotActive},
		configuredMiB:           4096,
		tolerateDeviceNotActive: true,
	}

	held, err := (balloonReclaim{
		vms:      map[string]balloon.Balloonable{"a": pending},
		floorMiB: 1024,
	}).apply(512)
	if !errors.Is(err, balloon.ErrTargetPending) {
		t.Fatalf("apply() = %v, want ErrTargetPending: an inactive device released nothing", err)
	}
	if held != 0 {
		t.Fatalf("apply() held=%d, want 0 for a reclaim that has not happened", held)
	}
}

func TestBalloonReclaimHoldsOnlyWhatActiveGuestsGaveBack(t *testing.T) {
	pending := balloonMachine{
		machine:                 &fakeMachine{setMemoryErr: hypervisor.ErrDeviceNotActive},
		configuredMiB:           4096,
		tolerateDeviceNotActive: true,
	}
	active := &fakeMachine{active: true}
	healthy := balloonMachine{machine: active, configuredMiB: 4096}

	held, err := (balloonReclaim{
		vms:      map[string]balloon.Balloonable{"a": pending, "b": healthy},
		floorMiB: 1024,
	}).apply(1024)
	if !errors.Is(err, balloon.ErrTargetPending) {
		t.Fatalf("apply() = %v, want ErrTargetPending naming the pending guest", err)
	}
	if !strings.Contains(err.Error(), "a") {
		t.Fatalf("apply() = %v, want the pending guest named", err)
	}
	// The healthy guest was asked for its share (half of 1024 + 1 MiB
	// residual) and only that share is held.
	want := 4096 - active.memoryTargets[len(active.memoryTargets)-1]
	if held != want || want <= 0 {
		t.Fatalf("apply() held=%d, want %d (the healthy guest's reduction only)", held, want)
	}
}
