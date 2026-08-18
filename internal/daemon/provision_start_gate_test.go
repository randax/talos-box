package daemon

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostpressure"
)

// nearlyFullSwap is the #334 reading: 7.3 GiB of an 8 GiB swap file used while
// the kernel still reports no elevated pressure, which is why the steady-state
// guard passed.
func nearlyFullSwap(string) (hostpressure.Snapshot, error) {
	return hostpressure.Snapshot{
		Swap:           hostpressure.Usage{TotalBytes: 8 << 30, AvailableBytes: 7 << 30 / 10},
		MemoryPressure: hostpressure.MemoryPressureNormal,
	}, nil
}

// TestCheckProvisionStart covers the admit/refuse boundaries of the
// provision-start gate with every host probe stubbed: the runner's real RAM and
// swap must never decide the verdict.
func TestCheckProvisionStart(t *testing.T) {
	reserve := balloon.DefaultConfig().ReserveMiB
	tests := []struct {
		name string
		// clusterRunning selects whether the fixture cluster's guests are running.
		clusterRunning bool
		addMiB         int
		freeMiB        int
		pressure       func(string) (hostpressure.Snapshot, error)
		force          bool
		wantErr        string
		wantWarning    string
	}{
		{
			name:           "second bringup without headroom is refused",
			clusterRunning: true,
			addMiB:         2048,
			freeMiB:        2048 + reserve - 1,
			pressure:       noHostPressure,
			wantErr:        "guests are already running",
		},
		{
			name:           "second bringup with headroom is admitted",
			clusterRunning: true,
			addMiB:         2048,
			freeMiB:        2048 + reserve,
			pressure:       noHostPressure,
		},
		{
			name:           "first bringup on a tight host is left to the balloon reserve",
			clusterRunning: false,
			addMiB:         2048,
			freeMiB:        1,
			pressure:       noHostPressure,
		},
		{
			name:           "nearly full swap refuses a second bringup with memory to spare",
			clusterRunning: true,
			addMiB:         2048,
			freeMiB:        1 << 20,
			pressure:       nearlyFullSwap,
			wantErr:        "host swap is 91% used",
		},
		{
			name:           "sticky swap on an idle host still admits (#231)",
			clusterRunning: false,
			addMiB:         2048,
			freeMiB:        1 << 20,
			pressure:       nearlyFullSwap,
		},
		{
			name:           "force downgrades the refusal to a warning",
			clusterRunning: true,
			addMiB:         2048,
			freeMiB:        2048 + reserve - 1,
			pressure:       noHostPressure,
			force:          true,
			wantWarning:    "(forced)",
		},
		{
			name:           "an unreadable pressure probe never blocks",
			clusterRunning: false,
			addMiB:         2048,
			freeMiB:        1 << 20,
			pressure:       failingHostPressure,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
			if !test.clusterRunning {
				delete(service.vms, item.Name)
			}
			service.hostPressure = test.pressure
			service.hostFreeMemory = func() (int, error) { return test.freeMiB, nil }
			service.hostTotalMemory = plentifulHostMemory

			warnings, err := service.checkProvisionStart(t.TempDir(), test.addMiB, test.force)

			if test.wantErr == "" && err != nil {
				t.Fatalf("checkProvisionStart() = %v, want admission", err)
			}
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("checkProvisionStart() admitted, want refusal containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("checkProvisionStart() error = %v, want %q", err, test.wantErr)
				}
				if !strings.Contains(err.Error(), "--force") {
					t.Errorf("checkProvisionStart() error = %v, want the --force override hint", err)
				}
			}
			if test.wantWarning != "" && !strings.Contains(strings.Join(warnings, "\n"), test.wantWarning) {
				t.Fatalf("checkProvisionStart() warnings = %q, want %q", warnings, test.wantWarning)
			}
		})
	}
}

func failingHostPressure(string) (hostpressure.Snapshot, error) {
	return hostpressure.Snapshot{}, errors.New("sysctl vm.swapusage: no such file")
}

// TestCreateClusterRefusesConcurrentBringupBeforeMutation is the #334 scenario
// end to end: one cluster is already running, the overcommit ceiling is wide
// open, and the host-pressure snapshot is clean — the create must still be
// refused, and must leave nothing behind.
func TestCreateClusterRefusesConcurrentBringupBeforeMutation(t *testing.T) {
	service, _ := runningLonghornClusterForNodeMutation(t, 1, 2)
	service.hostPressure = noHostPressure
	service.hostTotalMemory = plentifulHostMemory
	service.hostFreeMemory = func() (int, error) { return balloon.DefaultConfig().ReserveMiB, nil }
	service.helperCheck = func() error { return nil }
	raw, err := json.Marshal(createArgs{Name: "second", NodeDefaults: cluster.NodeDefaults{MemoryMiB: 2048, DiskGiB: 1}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.createCluster(raw, nil)

	if err == nil || !strings.Contains(err.Error(), "guests are already running") {
		t.Fatalf("createCluster() error = %v, want the projected-start refusal", err)
	}
	if _, loadErr := cluster.Load("second"); loadErr == nil {
		t.Fatal("createCluster() persisted state despite the projected-start refusal")
	}
}

// The same host that refuses a second bringup must still admit it when forced,
// since --force is the documented override for every other guest-start gate.
func TestCreateClusterForcedConcurrentBringupWarns(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	service.hostPressure = noHostPressure
	service.hostTotalMemory = plentifulHostMemory
	service.hostFreeMemory = func() (int, error) { return balloon.DefaultConfig().ReserveMiB, nil }
	service.helperCheck = func() error { return nil }
	warnings, err := service.checkProvisionStart(t.TempDir(), clusterMemoryMiB(item)+2048, true)
	if err != nil {
		t.Fatalf("forced checkProvisionStart() = %v, want admission", err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "guests are already running") || !strings.Contains(joined, "(forced)") {
		t.Fatalf("forced checkProvisionStart() warnings = %q, want the forced projected-start finding", warnings)
	}
}
