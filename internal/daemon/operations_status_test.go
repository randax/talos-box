package daemon

import (
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

func TestNodeStatusDoesNotProbeStoppedReservedIP(t *testing.T) {
	t.Parallel()

	node := cluster.Node{Name: "demo-cp-1", Role: cluster.RoleControlPlane, MAC: "52:54:00:00:00:02"}
	probeCalled := false
	status := nodeStatusWith(
		node,
		3,
		false,
		func(string, int) string { return "172.30.3.2" },
		func(string) ProbeResult {
			probeCalled = true
			return ProbeResult{Dialed: true, TLS: true}
		},
	)

	if probeCalled {
		t.Fatal("nodeStatusWith() probed apid for a stopped VM")
	}
	if status.IP != "172.30.3.2" {
		t.Fatalf("status IP = %q, want reserved IP", status.IP)
	}
	if status.APIDReachable {
		t.Fatal("stopped node reported apid reachable")
	}
	if status.Phase != PhaseStopped {
		t.Fatalf("status phase = %q, want %q", status.Phase, PhaseStopped)
	}
}

func TestNodeStatusProbesRunningReservedIP(t *testing.T) {
	t.Parallel()

	node := cluster.Node{Name: "demo-cp-1", Role: cluster.RoleControlPlane, MAC: "52:54:00:00:00:02"}
	probeCalled := false
	status := nodeStatusWith(
		node,
		3,
		true,
		func(string, int) string { return "172.30.3.2" },
		func(ip string) ProbeResult {
			probeCalled = true
			if ip != "172.30.3.2" {
				t.Fatalf("probed IP = %q, want reserved IP", ip)
			}
			return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}
		},
	)

	if !probeCalled {
		t.Fatal("nodeStatusWith() did not probe apid for a running VM")
	}
	if !status.APIDReachable || status.Phase != PhaseMaintenance {
		t.Fatalf("status = %+v, want reachable maintenance node", status)
	}
}

func TestStatusReportsPerClusterTalosVersionAndSchematic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, seed := range []struct {
		name      string
		version   string
		schematic string
	}{
		{"stable", "v1.13.6", "aaa"},
		{"canary", "v1.14.0", "bbb"},
	} {
		item, err := cluster.New(seed.name, 0, 1, 0, cluster.NodeDefaults{})
		if err != nil {
			t.Fatal(err)
		}
		item.TalosVersion = seed.version
		item.Schematic = seed.schematic
		if err := cluster.Save(item); err != nil {
			t.Fatal(err)
		}
	}

	service := &Server{vms: make(map[string]map[string]hypervisor.Machine)}
	statuses, err := service.status(mustRawJSON(t, statusArgs{}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]string{}
	for _, status := range statuses {
		got[status.Name] = [2]string{status.TalosVersion, status.Schematic}
	}
	if got["stable"] != [2]string{"v1.13.6", "aaa"} || got["canary"] != [2]string{"v1.14.0", "bbb"} {
		t.Fatalf("status talos identity = %v", got)
	}
}
