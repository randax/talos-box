package daemon

import (
	"os"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

func TestRestartSafeSuspendSummaryRequiresSuspendedCapableBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clusters := []struct {
		name       string
		subnet     int
		hypervisor hypervisor.Name
		suspended  bool
	}{
		{name: "qemu-suspended", subnet: 0, hypervisor: hypervisor.NameQEMU, suspended: true},
		{name: "vz-suspended", subnet: 1, hypervisor: hypervisor.NameVZ, suspended: true},
		{name: "qemu-running", subnet: 2, hypervisor: hypervisor.NameQEMU},
		{name: "unknown-suspended", subnet: 3, hypervisor: "missing", suspended: true},
	}
	for _, fixture := range clusters {
		item, err := cluster.New(fixture.name, fixture.subnet, 1, 0, cluster.NodeDefaults{})
		if err != nil {
			t.Fatal(err)
		}
		item.Hypervisor = string(fixture.hypervisor)
		if err := cluster.Save(item); err != nil {
			t.Fatal(err)
		}
		if fixture.suspended {
			dir, err := cluster.Dir(item.Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(saveStatePath(dir, item.Nodes[0].Name), []byte("state"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	service := &Server{
		hypervisors: fakeRegistry(hypervisor.NameQEMU, map[hypervisor.Name]hypervisor.Hypervisor{
			hypervisor.NameQEMU: &fakeHypervisor{capabilities: hypervisor.Capabilities{SuspendSurvivesDaemonRestart: true}},
			hypervisor.NameVZ:   &fakeHypervisor{},
		}),
		vms: map[string]map[string]hypervisor.Machine{
			"qemu-running": {"qemu-running-cp-1": &fakeMachine{active: true}},
		},
	}

	summaries, err := service.listClusters()
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]ClusterSummary, len(summaries))
	for _, summary := range summaries {
		byName[summary.Name] = summary
	}
	for name, want := range map[string]bool{
		"qemu-suspended":    true,
		"vz-suspended":      false,
		"qemu-running":      false,
		"unknown-suspended": false,
	} {
		if got := byName[name].SuspendSurvivesDaemonRestart; got != want {
			t.Errorf("%s SuspendSurvivesDaemonRestart = %t, want %t", name, got, want)
		}
	}
}
