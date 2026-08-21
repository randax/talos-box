package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
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

			warnings, _, err := service.checkProvisionStart(t.TempDir(), test.addMiB, test.force)

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
	warnings, _, err := service.checkProvisionStart(t.TempDir(), clusterMemoryMiB(item)+2048, true)
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
					_, _, err = service.startNodeLocked(raw, nil)
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

// balloonableFixture re-sizes the shared node-mutation fixture to guest memory
// the balloon controller can actually take back: the fixture's 1 MiB nodes sit
// below the per-node floor, so nothing about them is reclaimable and the
// reclaim path never arms. It also reports balloon readback, which is what lets
// Balloonables answer without probing apid on a guest that does not exist.
func balloonableFixture(t *testing.T, memoryMiB int) (*Server, cluster.Cluster) {
	t.Helper()
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	item.NodeDefaults.MemoryMiB = memoryMiB
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service.hypervisor = &fakeHypervisor{
		architecture: hypervisor.ArchitectureARM64,
		capabilities: hypervisor.Capabilities{BalloonReadback: hypervisor.FeatureStatus{Supported: true}},
	}
	service.hostPressure = noHostPressure
	service.hostTotalMemory = plentifulHostMemory
	return service, item
}

// #398: a shortfall the already-running guests can give back is not a refusal.
// The gate pre-balloons exactly the shortfall out of them before the new guests
// boot, which is the --force-free path forward the incident had none of.
func TestProvisionStartPreBalloonsRunningGuestsInsteadOfRefusing(t *testing.T) {
	const nodeMiB = 2048
	reserve := balloon.DefaultConfig().ReserveMiB
	floor := balloon.DefaultConfig().FloorMiB
	service, _ := balloonableFixture(t, nodeMiB)
	const shortfall = 512
	service.hostFreeMemory = func() (int, error) { return reserve + nodeMiB - shortfall, nil }

	warnings, _, err := service.checkProvisionStart(t.TempDir(), nodeMiB, false)

	if err != nil {
		t.Fatalf("checkProvisionStart() = %v, want admission after pre-ballooning", err)
	}
	if joined := strings.Join(warnings, "\n"); !strings.Contains(joined, "pre-ballooned 512 MiB") {
		t.Fatalf("checkProvisionStart() warnings = %q, want the pre-balloon narration", warnings)
	}
	reclaimed := 0
	for _, machine := range service.vms["demo"] {
		fake, ok := machine.(*fakeMachine)
		if !ok || len(fake.memoryTargets) == 0 {
			t.Fatalf("node was never ballooned down: %#v", machine)
		}
		target := fake.memoryTargets[len(fake.memoryTargets)-1]
		if target < floor {
			t.Fatalf("balloon target %d MiB is below the %d MiB per-node floor", target, floor)
		}
		reclaimed += nodeMiB - target
	}
	if reclaimed < shortfall {
		t.Fatalf("pre-balloon reclaimed %d MiB, want at least the %d MiB shortfall", reclaimed, shortfall)
	}
	// The hold must cover the memory that is actually out of the guests measured
	// from their configured size — the manager's reconcile anchors there, so a
	// hold of only the incremental shortfall lets the next poll inflate them back.
	if got := service.BalloonHoldMiB(); got != reclaimed {
		t.Fatalf("BalloonHoldMiB() = %d, want the %d MiB actually ballooned out so the manager cannot hand it straight back", got, reclaimed)
	}
}

// The hold is measured from the configured size, not from the shortfall this
// admission asked for: the balloon manager recomputes every target from
// ConfiguredMiB, so a hold covering only the increment hands back whatever an
// earlier reclaim had already taken, dropping the host below the reserve the
// gate just promised.
func TestProvisionStartHoldsTheCumulativeReclaimNotJustTheShortfall(t *testing.T) {
	const nodeMiB = 2048
	reserve := balloon.DefaultConfig().ReserveMiB
	service, _ := balloonableFixture(t, nodeMiB)
	// Every guest already sits 300 MiB below configured from an earlier reclaim.
	const alreadyOut = 300
	for name := range service.vms["demo"] {
		service.recordBalloonTarget("demo/"+name, nodeMiB-alreadyOut)
	}
	const shortfall = 512
	service.hostFreeMemory = func() (int, error) { return reserve + nodeMiB - shortfall, nil }

	if _, _, err := service.checkProvisionStart(t.TempDir(), nodeMiB, false); err != nil {
		t.Fatalf("checkProvisionStart() = %v, want admission after pre-ballooning", err)
	}

	reclaimed := 0
	for _, machine := range service.vms["demo"] {
		fake, ok := machine.(*fakeMachine)
		if !ok || len(fake.memoryTargets) == 0 {
			t.Fatalf("node was never ballooned down: %#v", machine)
		}
		reclaimed += nodeMiB - fake.memoryTargets[len(fake.memoryTargets)-1]
	}
	if reclaimed < shortfall+3*alreadyOut {
		t.Fatalf("pre-balloon left only %d MiB out of the guests, want the earlier %d MiB plus the %d MiB shortfall", reclaimed, 3*alreadyOut, shortfall)
	}
	if got := service.BalloonHoldMiB(); got != reclaimed {
		t.Fatalf("BalloonHoldMiB() = %d, want the cumulative %d MiB that is actually out of the guests", got, reclaimed)
	}
}

// #398 follow-up: measuring the balloon credit dials apid on every running node
// on backends without balloon readback, and the whole gate runs under opMu. A
// host with headroom to spare must never pay for a credit no shortfall can
// spend.
func TestProvisionStartSkipsTheBalloonCreditWhenTheHostHasHeadroom(t *testing.T) {
	const nodeMiB = 2048
	service, _ := balloonableFixture(t, nodeMiB)
	service.hostFreeMemory = func() (int, error) { return 1 << 20, nil }
	measured := 0
	service.balloonables = func() map[string]balloon.Balloonable {
		measured++
		return nil
	}

	if _, _, err := service.checkProvisionStart(t.TempDir(), nodeMiB, false); err != nil {
		t.Fatalf("checkProvisionStart() = %v, want admission on a roomy host", err)
	}
	if measured != 0 {
		t.Fatalf("balloon credit was measured %d times on a host with no shortfall, want 0", measured)
	}
}

// The credit is bounded by what the running guests can actually give back: past
// it the gate still refuses, and says the pre-balloon was already counted so the
// operator does not go looking for headroom the gate ignored.
func TestProvisionStartRefusesShortfallBeyondReclaimableMemory(t *testing.T) {
	const nodeMiB = 2048
	reserve := balloon.DefaultConfig().ReserveMiB
	floor := balloon.DefaultConfig().FloorMiB
	service, _ := balloonableFixture(t, nodeMiB)
	reclaimable := 3 * (nodeMiB - floor)
	service.hostFreeMemory = func() (int, error) { return reserve + nodeMiB - reclaimable - 1, nil }

	_, _, err := service.checkProvisionStart(t.TempDir(), nodeMiB, false)

	if err == nil {
		t.Fatal("checkProvisionStart() admitted a shortfall past the reclaimable memory")
	}
	if !strings.Contains(err.Error(), "the running guests can give back") {
		t.Fatalf("checkProvisionStart() error = %v, want the refusal to name the balloon credit", err)
	}
	if got := service.BalloonHoldMiB(); got != 0 {
		t.Fatalf("BalloonHoldMiB() = %d after a refusal, want no hold", got)
	}
}

// A guest that cannot be ballooned is not headroom: if the reclaim fails to
// apply, the start is refused rather than admitted on memory nothing gave back.
func TestProvisionStartRefusesWhenThePreBalloonFails(t *testing.T) {
	const nodeMiB = 2048
	reserve := balloon.DefaultConfig().ReserveMiB
	service, _ := balloonableFixture(t, nodeMiB)
	service.hostFreeMemory = func() (int, error) { return reserve + nodeMiB - 512, nil }
	for _, machine := range service.vms["demo"] {
		machine.(*fakeMachine).setMemoryErr = errors.New("virtio_balloon not ready")
	}

	_, _, err := service.checkProvisionStart(t.TempDir(), nodeMiB, false)

	if err == nil || !strings.Contains(err.Error(), "virtio_balloon not ready") {
		t.Fatalf("checkProvisionStart() error = %v, want the failed pre-balloon to refuse", err)
	}
}

// The whole provisioning dispatch — create, start, up, and every node mutation
// — runs under opMu, so the gate and the pre-balloon it takes must never lock
// it again. A regression here does not fail, it hangs.
func TestProvisionStartGateRunsUnderOpMu(t *testing.T) {
	const nodeMiB = 2048
	service, _ := balloonableFixture(t, nodeMiB)
	service.hostFreeMemory = func() (int, error) { return balloon.DefaultConfig().ReserveMiB + nodeMiB - 512, nil }

	service.opMu.Lock()
	defer service.opMu.Unlock()

	if _, _, err := service.checkProvisionStart(t.TempDir(), nodeMiB, false); err != nil {
		t.Fatalf("checkProvisionStart() under opMu = %v, want admission after pre-ballooning", err)
	}
}

// shrinkGuests puts every running guest at targetMiB the way the balloon
// manager does — through the Balloonables the manager reconciles — so the
// daemon records the target it applied.
func shrinkGuests(t *testing.T, service *Server, targetMiB int) {
	t.Helper()
	for name, vm := range service.Balloonables() {
		if err := vm.SetMemoryTargetMiB(targetMiB); err != nil {
			t.Fatalf("SetMemoryTargetMiB(%s) = %v", name, err)
		}
	}
}

// Memory the balloon manager has already reclaimed is in the host-free reading
// the gate measures, so counting each guest's whole configured size above the
// floor as still-reclaimable credits it twice and admits starts on headroom
// that does not exist.
func TestProvisionStartCreditsOnlyMemoryTheGuestsStillHold(t *testing.T) {
	const nodeMiB, shrunkMiB = 2048, 1200
	floor := balloon.DefaultConfig().FloorMiB
	service, _ := balloonableFixture(t, nodeMiB)
	shrinkGuests(t, service, shrunkMiB)

	nodes := len(service.vms["demo"])
	want := nodes * (shrunkMiB - floor)
	if got := service.balloonReclaim().availableMiB; got != want {
		t.Fatalf("balloonReclaim().availableMiB = %d, want %d — only what the shrunk guests still hold above the floor", got, want)
	}

	// A shortfall past that credit must refuse rather than promise a reclaim
	// the guests cannot make a second time.
	reserve := balloon.DefaultConfig().ReserveMiB
	service.hostFreeMemory = func() (int, error) { return reserve + nodeMiB - want - 1, nil }
	if _, _, err := service.checkProvisionStart(t.TempDir(), nodeMiB, false); err == nil {
		t.Fatal("checkProvisionStart() admitted a shortfall past the memory the guests still hold")
	}
}

// A pre-balloon may only take memory back. Planning from the configured size
// hands memory to guests the manager had already shrunk, lowering host free
// memory in the moment the gate promised to raise it.
func TestProvisionStartPreBalloonNeverInflatesAlreadyShrunkGuests(t *testing.T) {
	const nodeMiB, shrunkMiB, shortfall = 2048, 1200, 256
	reserve := balloon.DefaultConfig().ReserveMiB
	service, _ := balloonableFixture(t, nodeMiB)
	shrinkGuests(t, service, shrunkMiB)
	service.hostFreeMemory = func() (int, error) { return reserve + nodeMiB - shortfall, nil }

	if _, _, err := service.checkProvisionStart(t.TempDir(), nodeMiB, false); err != nil {
		t.Fatalf("checkProvisionStart() = %v, want admission after pre-ballooning", err)
	}

	reclaimed := 0
	for name, machine := range service.vms["demo"] {
		fake := machine.(*fakeMachine)
		target := fake.memoryTargets[len(fake.memoryTargets)-1]
		if target > shrunkMiB {
			t.Fatalf("node %s was inflated from %d MiB to %d MiB by a pre-balloon", name, shrunkMiB, target)
		}
		reclaimed += shrunkMiB - target
	}
	if reclaimed < shortfall {
		t.Fatalf("pre-balloon reclaimed %d MiB, want at least the %d MiB shortfall", reclaimed, shortfall)
	}
}

// #398: the pre-balloon hold is a boot-window budget, so its clock has to start
// at the launch rather than at the admission. A cold create spends an unbounded
// image fetch and a disk clone per node between the two; if the hold expires in
// there, the balloon manager inflates the reclaimed guests back before the
// admitted ones have booted — exactly the concurrent-bringup squeeze the
// pre-balloon was taken to prevent.
func TestCreateClusterRearmsThePreBalloonHoldAtLaunch(t *testing.T) {
	const nodeMiB, shortfall = 2048, 512
	reserve := balloon.DefaultConfig().ReserveMiB
	service, _ := balloonableFixture(t, nodeMiB)
	stubNodeMutationReconcile(service)
	service.helperCheck = func() error { return nil }
	service.hostFreeMemory = func() (int, error) { return reserve + nodeMiB - shortfall, nil }
	schematic := "second-schematic"
	seedCachedDisk(t, schematic, DefaultTalosVersion)

	raw, err := json.Marshal(createArgs{
		Name:          "second",
		ControlPlanes: intPtr(1),
		Workers:       intPtr(0),
		Schematic:     schematic,
		NodeDefaults:  cluster.NodeDefaults{MemoryMiB: nodeMiB, DiskGiB: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stands in for the slow cold create: the admission-time hold runs out
	// while the create is still cloning disks, before anything has launched.
	cloned := false
	expireHold := stageFunc(func(line string) {
		if !strings.Contains(line, "cloning") {
			return
		}
		cloned = true
		service.balloonHoldMu.Lock()
		for i := range service.balloonHolds {
			service.balloonHolds[i].until = time.Now().Add(-time.Second)
		}
		service.balloonHoldMu.Unlock()
	})

	if _, err := service.createCluster(raw, expireHold); err != nil {
		t.Fatalf("createCluster() = %v, want admission after pre-ballooning", err)
	}

	if !cloned {
		t.Fatal("the create never reached the disk-clone stage; the fixture no longer exercises the window this test is about")
	}
	if got := service.BalloonHoldMiB(); got <= 0 {
		t.Fatalf("BalloonHoldMiB() = %d after the create launched its guests, want the pre-balloon still held: an expired hold lets the balloon manager hand the reclaim back before the admitted guests boot", got)
	}
}

func intPtr(v int) *int { return &v }

// seedCachedDisk puts a cached image where cache.Ensure will find it, so a
// create in these tests never reaches the Image Factory.
func seedCachedDisk(t *testing.T, schematic, version string) {
	t.Helper()
	// The shared fixture roots its cache at $HOME/cache, and HOME is the test's
	// own temp dir, so this stays inside the sandbox.
	path := filepath.Join(os.Getenv("HOME"), "cache", schematic, version, string(hypervisor.ArchitectureARM64), "disk.raw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// #398: the hold manufactures a reconcile deficit that PlanTargets spreads over
// whatever is balloonable at that tick, so a hold that outlives the guests it
// was taken from squeezes guests that were never part of the reclaim. Stopping
// the reclaimed cluster must take its share of the hold with it.
func TestBalloonHoldNeverOutlivesTheGuestsItWasTakenFrom(t *testing.T) {
	const nodeMiB, shortfall = 2048, 512
	reserve := balloon.DefaultConfig().ReserveMiB
	service, _ := balloonableFixture(t, nodeMiB)
	service.hostFreeMemory = func() (int, error) { return reserve + nodeMiB - shortfall, nil }

	if _, _, err := service.checkProvisionStart(t.TempDir(), nodeMiB, false); err != nil {
		t.Fatalf("checkProvisionStart() = %v, want admission after pre-ballooning", err)
	}
	if got := service.BalloonHoldMiB(); got < shortfall {
		t.Fatalf("BalloonHoldMiB() = %d right after the pre-balloon, want at least the %d MiB reclaimed", got, shortfall)
	}

	// The reclaimed cluster stops: its memory is back on the host, and nothing
	// is being held out of any guest any more.
	delete(service.vms, "demo")
	if got := service.BalloonHoldMiB(); got != 0 {
		t.Fatalf("BalloonHoldMiB() = %d after the reclaimed guests stopped, want 0 — a stale hold pins unrelated guests at the balloon floor on a host with memory to spare", got)
	}
}

// #398: the hold is armed at admission but only re-armed at the launch. Every
// failure in between is a start that never happened, and it must not keep
// memory out of the running guests for the rest of the TTL.
func TestCreateClusterReleasesThePreBalloonHoldWhenItFailsBeforeLaunch(t *testing.T) {
	const nodeMiB, shortfall = 2048, 512
	reserve := balloon.DefaultConfig().ReserveMiB
	service, item := balloonableFixture(t, nodeMiB)
	stubNodeMutationReconcile(service)
	service.helperCheck = func() error { return nil }
	service.hostFreeMemory = func() (int, error) { return reserve + nodeMiB - shortfall, nil }

	// A domain already taken by the running cluster: refused after the gate has
	// already pre-ballooned, and long before anything launches.
	raw, err := json.Marshal(createArgs{
		Name:          "second",
		ControlPlanes: intPtr(1),
		Workers:       intPtr(0),
		Domain:        item.Name + "." + cluster.DefaultDomainSuffix,
		NodeDefaults:  cluster.NodeDefaults{MemoryMiB: nodeMiB, DiskGiB: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.createCluster(raw, nil); err == nil {
		t.Fatal("createCluster() accepted a domain already in use; the test no longer exercises a failure between the gate and the launch")
	}
	if got := service.BalloonHoldMiB(); got != 0 {
		t.Fatalf("BalloonHoldMiB() = %d after a create that never launched, want 0", got)
	}
}

// #398: a create slow enough that the admission-time hold expires re-arms the
// hold at its launch. By then the balloon manager has legitimately inflated the
// reclaimed guests back to configured, so nothing is out of them any more — and
// the re-armed hold is precisely the instruction to take it again. Clamping it
// to what is currently out would zero it in exactly the case it was built for.
func TestBalloonHoldSurvivesAReArmAfterTheManagerHandedTheReclaimBack(t *testing.T) {
	const nodeMiB, shortfall = 2048, 512
	reserve := balloon.DefaultConfig().ReserveMiB
	service, _ := balloonableFixture(t, nodeMiB)
	service.hostFreeMemory = func() (int, error) { return reserve + nodeMiB - shortfall, nil }

	_, heldMiB, err := service.checkProvisionStart(t.TempDir(), nodeMiB, false)
	if err != nil {
		t.Fatalf("checkProvisionStart() = %v, want admission after pre-ballooning", err)
	}
	if heldMiB <= 0 {
		t.Fatalf("checkProvisionStart() held %d MiB, want the pre-balloon it applied", heldMiB)
	}

	// The admission hold runs out while the create is still cloning disks, and
	// the manager's next tick puts every guest back at its configured size.
	service.balloonHoldMu.Lock()
	for i := range service.balloonHolds {
		service.balloonHolds[i].until = time.Now().Add(-time.Second)
	}
	service.balloonHoldMu.Unlock()
	for name := range service.vms["demo"] {
		service.recordBalloonTarget("demo/"+name, nodeMiB)
	}

	service.holdBalloonReclaim(heldMiB)

	if got := service.BalloonHoldMiB(); got != heldMiB {
		t.Fatalf("BalloonHoldMiB() = %d after the launch re-armed a %d MiB hold, want it live: the admitted guests boot on headroom the manager must take back", got, heldMiB)
	}
}

// #398: the hold outlives the operation that took it while the admitted guests
// boot, so two starts overlap. A start that fails before launching must hand
// back only its own hold — dropping an earlier, still-live one lets the manager
// inflate that start's guests back mid-boot.
func TestReleaseBalloonHoldLeavesAnOverlappingStartsHoldStanding(t *testing.T) {
	const nodeMiB = 2048
	service, _ := balloonableFixture(t, nodeMiB)

	// Start A pre-balloons 512 MiB and launches; its guests are booting.
	service.holdBalloonReclaim(512)
	// Start B runs the gate 30s later; its hold is cumulative, so it subsumes A's.
	service.holdBalloonReclaim(900)
	// B fails before it launches.
	service.releaseBalloonHold(900)

	if got := service.BalloonHoldMiB(); got != 512 {
		t.Fatalf("BalloonHoldMiB() = %d after an overlapping start released its own hold, want A's 512 MiB still held while A's guests boot", got)
	}
}

// #398 asks for one gate and one arithmetic across `up`, `cluster create` and
// `cluster start`. A spec that raises one role above the cluster-wide `node:`
// block must be charged at its real footprint on the create path too: charging
// the flat node default admits a cluster that the later `cluster start` — which
// resolves memory through Cluster.DefaultsFor — correctly refuses.
func TestCreateClusterChargesPerRoleMemoryOverrides(t *testing.T) {
	const nodeMiB, controlPlaneMiB = 4096, 8192
	reserve := balloon.DefaultConfig().ReserveMiB
	service, _ := runningLonghornClusterForNodeMutation(t, 1, 2)
	service.hostPressure = noHostPressure
	service.hostTotalMemory = plentifulHostMemory
	service.helperCheck = func() error { return nil }
	// Exactly enough for the flat charge (3 x 4096) and 4096 MiB short of the
	// real footprint (8192 + 2 x 4096).
	service.hostFreeMemory = func() (int, error) { return reserve + 3*nodeMiB, nil }
	// Seeded so that an admitted create would go on to build the cluster from
	// the local cache: this test must never reach the Image Factory, whichever
	// way the gate rules.
	schematic := "role-override-schematic"
	seedCachedDisk(t, schematic, DefaultTalosVersion)
	raw, err := json.Marshal(createArgs{
		Name:          "second",
		ControlPlanes: intPtr(1),
		Workers:       intPtr(2),
		Schematic:     schematic,
		NodeDefaults:  cluster.NodeDefaults{MemoryMiB: nodeMiB, DiskGiB: 1},
		ControlPlane:  &cluster.NodeDefaults{MemoryMiB: controlPlaneMiB, DiskGiB: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.createCluster(raw, nil)

	if err == nil || !strings.Contains(err.Error(), "guests are already running") {
		t.Fatalf("createCluster() error = %v, want the projected-start refusal: the gate must charge the controlPlane override, not the flat node default", err)
	}
	if _, loadErr := cluster.Load("second"); loadErr == nil {
		t.Fatal("createCluster() persisted state despite the projected-start refusal")
	}
}

// The mirror of the same rule: with no per-role override the create charges the
// cluster-wide node defaults, unchanged.
func TestRoleMemoryMiBFallsBackToNodeDefaults(t *testing.T) {
	base := cluster.NodeDefaults{MemoryMiB: 4096}
	if got := roleMemoryMiB(nil, base); got != 4096 {
		t.Fatalf("roleMemoryMiB(nil, %d) = %d, want the node default", base.MemoryMiB, got)
	}
	if got := roleMemoryMiB(&cluster.NodeDefaults{MemoryMiB: 8192}, base); got != 8192 {
		t.Fatalf("roleMemoryMiB(override) = %d, want the override", got)
	}
	if got := roleMemoryMiB(&cluster.NodeDefaults{CPUs: 4}, base); got != 4096 {
		t.Fatalf("roleMemoryMiB(memory-less override) = %d, want the node default", got)
	}
	if got := roleMemoryMiB(nil, cluster.NodeDefaults{}); got != cluster.DefaultMemoryMiB {
		t.Fatalf("roleMemoryMiB(nil, zero) = %d, want the built-in default", got)
	}
}
