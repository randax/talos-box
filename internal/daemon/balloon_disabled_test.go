package daemon

import (
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// A daemon built with TBX_DISABLE_BALLOON launches balloon-less guests, hides
// them from the manager and counts no reclaimable memory (#513).
func TestDisabledBalloonLaunchesWithoutDeviceAndReclaimsNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("no-balloon", 0, 1, 0, cluster.NodeDefaults{CPUs: 2, MemoryMiB: 4096})
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeHypervisor{capabilities: hypervisor.Capabilities{
		BalloonReadback: hypervisor.FeatureStatus{Supported: true},
	}}
	service := &Server{
		hypervisors:     singleFakeRegistry(backend),
		vms:             make(map[string]map[string]hypervisor.Machine),
		subnetSources:   emptySubnetSources(),
		balloonDisabled: true,
	}
	if _, err := service.start(item); err != nil {
		t.Fatal(err)
	}
	if len(backend.specs) != 1 || !backend.specs[0].DisableBalloon {
		t.Fatalf("Launch specs = %+v, want one with DisableBalloon", backend.specs)
	}
	if got := service.Balloonables(); len(got) != 0 {
		t.Fatalf("Balloonables() = %d nodes, want none with the balloon disabled", len(got))
	}
	if reclaim := service.balloonReclaim(); reclaim.availableMiB != 0 {
		t.Fatalf("balloonReclaim().availableMiB = %d, want 0", reclaim.availableMiB)
	}
	info, err := service.handle(Request{Op: "daemon.info"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !info.(Info).BalloonDisabled {
		t.Fatalf("daemon.info = %+v, want BalloonDisabled", info)
	}
}
