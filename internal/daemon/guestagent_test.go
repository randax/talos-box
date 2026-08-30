package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

func TestLaunchRequestsGuestAgentChannelOnlyWhenExtensionRequested(t *testing.T) {
	tests := []struct {
		name       string
		extensions []string
		want       bool
	}{
		{name: "requested", extensions: []string{"gvisor", "qemu-guest-agent"}, want: true},
		{name: "other extensions", extensions: []string{"gvisor"}},
		{name: "none"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			item, err := cluster.New(test.name, 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
			if err != nil {
				t.Fatal(err)
			}
			item.TalosExtensions = test.extensions
			backend := &fakeHypervisor{}
			service := &Server{
				hypervisors:   singleFakeRegistry(backend),
				vms:           make(map[string]map[string]hypervisor.Machine),
				subnetSources: emptySubnetSources(),
			}
			if _, err := service.launchMachine(item, item.Nodes[0], nil); err != nil {
				t.Fatal(err)
			}
			dir, err := cluster.Dir(item.Name)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if test.want {
				want = filepath.Join(dir, item.Nodes[0].Name+".qga.sock")
			}
			if got := backend.specs[0].GuestAgentSocketPath; got != want {
				t.Fatalf("GuestAgentSocketPath = %q, want %q", got, want)
			}
		})
	}
}

func TestStatusGatesGuestAgentOnBackendsWithoutTheChannel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("gated", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosExtensions = []string{"qemu-guest-agent"}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	const reason = "this backend has no guest-agent channel"
	service := &Server{
		hypervisors: singleFakeRegistry(&fakeHypervisor{capabilities: hypervisor.Capabilities{
			GuestAgent: hypervisor.FeatureStatus{Reason: reason},
		}}),
		vms: make(map[string]map[string]hypervisor.Machine),
	}
	statuses, err := service.status(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []CapabilityStatus{{Name: "qemu-guest-agent", Reason: reason}}
	if got := statuses[0].Capabilities; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Capabilities = %+v, want %+v", got, want)
	}
	hinted := false
	for _, hint := range statuses[0].Hints {
		if strings.Contains(hint, "qemu-guest-agent is unavailable on this host: "+reason) {
			hinted = true
		}
	}
	if !hinted {
		t.Fatalf("Hints = %v, want a capability-gate hint", statuses[0].Hints)
	}
}

func TestStatusReportsNoCapabilityGateWhenBackendSupportsTheChannel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("supported", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	item.TalosExtensions = []string{"qemu-guest-agent"}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		hypervisors: singleFakeRegistry(&fakeHypervisor{capabilities: hypervisor.Capabilities{
			GuestAgent: hypervisor.FeatureStatus{Supported: true},
		}}),
		vms: make(map[string]map[string]hypervisor.Machine),
	}
	statuses, err := service.status(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []CapabilityStatus{{Name: "qemu-guest-agent", Supported: true}}
	if got := statuses[0].Capabilities; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Capabilities = %+v, want %+v", got, want)
	}
	for _, hint := range statuses[0].Hints {
		if strings.Contains(hint, "unavailable on this host") {
			t.Fatalf("Hints = %v, want no capability-gate hint", statuses[0].Hints)
		}
	}
}
