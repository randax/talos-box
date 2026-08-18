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

// nearlyFullSwap is the #334 reading exactly as the incident records it: 7.3
// GiB of an 8 GiB swap file used, and host pressure back to *normal* by the
// time preflight read it — macOS keeps the file allocated long after the burst
// that filled it ends, which is why the steady-state guard reported PASS and
// the second create was admitted. The gate must refuse on the footprint plus a
// scarce free-memory reading, without waiting for the kernel to agree.
func nearlyFullSwap(string) (hostpressure.Snapshot, error) {
	return hostpressure.Snapshot{
		Swap:           hostpressure.Usage{TotalBytes: 8 << 30, AvailableBytes: 7 << 30 / 10},
		MemoryPressure: hostpressure.MemoryPressureNormal,
	}, nil
}

// elevatedPressureSmallSwap is the other half of the rule: a swapped-out
// footprint far too small to condemn a host on its own, on a kernel that is
// still reporting pressure. The percentage ceiling plus the live pressure
// reading is enough there.
func elevatedPressureSmallSwap(string) (hostpressure.Snapshot, error) {
	return hostpressure.Snapshot{
		Swap:           hostpressure.Usage{TotalBytes: 2 << 30, AvailableBytes: 2 << 30 * 18 / 100},
		MemoryPressure: hostpressure.MemoryPressureWarning,
	}, nil
}

// smallStickySwapfile is the everyday macOS reading: a 2 GiB dynamic swapfile
// mostly used, memory pressure normal, tens of gigabytes of RAM free. macOS
// keeps swap allocated long after the pressure that filled it cleared, so this
// says nothing about the host's capacity to boot another guest.
func smallStickySwapfile(string) (hostpressure.Snapshot, error) {
	return hostpressure.Snapshot{
		Swap:           hostpressure.Usage{TotalBytes: 2 << 30, AvailableBytes: 2 << 30 * 18 / 100},
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
			// The incident shape: the footprint is out on disk *and* the host has
			// less than half its RAM free, which is what separates live pressure
			// from a swap file macOS never returned. plentifulHostMemory is the
			// stubbed host total, so just under half of it is the scarce reading.
			name:           "the #334 swap footprint refuses a second bringup while RAM is scarce (#334)",
			clusterRunning: true,
			addMiB:         2048,
			freeMiB:        1<<19 - 1,
			pressure:       nearlyFullSwap,
			wantErr:        "host swap is 91% used",
		},
		{
			// The same footprint with most of the host's RAM free is #231's stale
			// swapfile at a larger size, and must not refuse a healthy create.
			name:           "the same footprint alongside free RAM admits (#231)",
			clusterRunning: true,
			addMiB:         2048,
			freeMiB:        1 << 20,
			pressure:       nearlyFullSwap,
		},
		{
			name:           "a small sticky swapfile at normal pressure admits (#231)",
			clusterRunning: true,
			addMiB:         2048,
			freeMiB:        1 << 20,
			pressure:       smallStickySwapfile,
		},
		{
			name:           "a small sticky swapfile under elevated pressure refuses",
			clusterRunning: true,
			addMiB:         2048,
			freeMiB:        1 << 20,
			pressure:       elevatedPressureSmallSwap,
			wantErr:        "host swap is 82% used",
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

// The refusal names how much guest memory is already resident, and an operator
// has to be able to re-derive that number. A partly-running cluster's stopped
// members are not resident, so counting the whole cluster would overstate it.
func TestRunningVMMemoryCountsOnlyRunningNodes(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	if got, want := service.runningVMMemoryMiB(), clusterMemoryMiB(item); got != want {
		t.Fatalf("runningVMMemoryMiB() = %d, want %d for a fully running cluster", got, want)
	}
	delete(service.vms[item.Name], "demo-worker-2")
	stopped := item.DefaultsFor(cluster.RoleWorker).MemoryMiB
	if got, want := service.runningVMMemoryMiB(), clusterMemoryMiB(item)-stopped; got != want {
		t.Fatalf("runningVMMemoryMiB() = %d, want %d once one member is stopped", got, want)
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

// The projected-start gate belongs to every verb that boots a guest, not only
// to create and whole-cluster start: `node add` on a running cluster and `node
// start` each commit a nominal allocation the host must actually have.
func TestNodeMutationVerbsAnswerToTheProvisionStartGate(t *testing.T) {
	reserve := balloon.DefaultConfig().ReserveMiB
	tests := []struct {
		name    string
		freeMiB int
		wantErr string
	}{
		{name: "admitted with headroom to spare", freeMiB: reserve + 4096},
		{name: "refused without headroom", freeMiB: reserve, wantErr: "guests are already running"},
	}
	for _, verb := range []string{"node.add", "node.start"} {
		for _, test := range tests {
			t.Run(verb+"/"+test.name, func(t *testing.T) {
				service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
				stubNodeMutationReconcile(service)
				service.hostPressure = noHostPressure
				service.hostTotalMemory = plentifulHostMemory
				service.hostFreeMemory = func() (int, error) { return test.freeMiB, nil }

				var err error
				if verb == "node.add" {
					raw, marshalErr := json.Marshal(nodeArgs{Cluster: item.Name, Role: cluster.RoleWorker})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					_, _, err = service.addNodeLocked(raw, nil)
				} else {
					// A stopped member is what node.start boots, and its memory
					// is entirely new to the free memory the gate measures.
					delete(service.vms[item.Name], "demo-worker-2")
					raw, marshalErr := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-2"})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					_, _, err = service.startNodeLocked(raw)
				}

				if test.wantErr == "" {
					if err != nil {
						t.Fatalf("%s error = %v, want admission", verb, err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("%s error = %v, want the projected-start refusal", verb, err)
				}
			})
		}
	}
}

// `cluster start` boots the stopped members of a partly-running cluster too, so
// the gate follows what it actually starts rather than whether the cluster as a
// whole is down.
func TestStartClusterGatesThePartlyStoppedMembersItBoots(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	service.hostPressure = noHostPressure
	service.hostTotalMemory = plentifulHostMemory
	service.hostFreeMemory = func() (int, error) { return balloon.DefaultConfig().ReserveMiB, nil }
	delete(service.vms[item.Name], "demo-worker-2")

	raw, err := json.Marshal(startArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.startCluster(raw)

	if err == nil || !strings.Contains(err.Error(), "guests are already running") {
		t.Fatalf("startCluster() error = %v, want the projected-start refusal for the stopped member", err)
	}
}

// The same start with nothing left to boot has no allocation to project, so the
// gate must not fire on memory that is already resident.
func TestStartClusterSkipsTheGateWhenEveryMemberIsRunning(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	service.hostPressure = noHostPressure
	service.hostTotalMemory = plentifulHostMemory
	service.hostFreeMemory = func() (int, error) { return 0, errors.New("unmeasurable") }
	if got := service.stoppedNodeMemoryMiB(item); got != 0 {
		t.Fatalf("stoppedNodeMemoryMiB() = %d, want 0 for a fully running cluster", got)
	}

	raw, err := json.Marshal(startArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.startCluster(raw); err != nil {
		t.Fatalf("startCluster() error = %v, want admission", err)
	}
}
