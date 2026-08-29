package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

func teardownTestServer(t *testing.T, machines ...hypervisor.Machine) (*Server, cluster.Cluster) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("teardown", 0, len(machines), 0, cluster.NodeDefaults{MemoryMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	nodes := map[string]hypervisor.Machine{}
	for i, machine := range machines {
		nodes[item.Nodes[i].Name] = machine
	}
	service := &Server{
		hypervisor: &fakeHypervisor{capabilities: hypervisor.Capabilities{
			BalloonReadback: hypervisor.FeatureStatus{Supported: true},
		}},
		vms: map[string]map[string]hypervisor.Machine{item.Name: nodes},
	}
	return service, item
}

// A teardown in flight pauses the manager's tick but keeps the node list
// intact: an empty snapshot would prune the manager's ledger for the clusters
// that stay up (#513).
func TestBalloonablesStayListedWhileTeardownPausesTicks(t *testing.T) {
	service, _ := teardownTestServer(t, &fakeMachine{active: true})
	release := service.quiesceBalloon()
	if !service.BalloonPaused() {
		t.Fatal("BalloonPaused() = false during teardown")
	}
	if got := service.Balloonables(); len(got) != 1 {
		t.Fatalf("Balloonables() during teardown = %d nodes, want 1", len(got))
	}
	release()
	if service.BalloonPaused() {
		t.Fatal("BalloonPaused() = true after teardown")
	}
}

// A snapshot the manager took before the teardown began must not retarget a
// guest that is now being stopped (#513).
func TestBalloonMachineRefusesRetargetDuringTeardown(t *testing.T) {
	machine := &fakeMachine{active: true}
	service, item := teardownTestServer(t, machine)
	vms := service.Balloonables()
	release := service.quiesceBalloon()
	err := vms[item.Name+"/"+item.Nodes[0].Name].SetMemoryTargetMiB(1024)
	if !errors.Is(err, balloon.ErrQuiesced) {
		t.Fatalf("SetMemoryTargetMiB() during teardown = %v, want balloon.ErrQuiesced", err)
	}
	if len(machine.memoryTargets) != 0 {
		t.Fatalf("machine retargeted during teardown: %v", machine.memoryTargets)
	}
	release()
	if err := vms[item.Name+"/"+item.Nodes[0].Name].SetMemoryTargetMiB(1024); err != nil {
		t.Fatalf("SetMemoryTargetMiB() after teardown = %v", err)
	}
}

// Shutdown parks the balloon manager for good before any VM is closed.
func TestShutdownQuiescesBalloonBeforeClosingVMs(t *testing.T) {
	var quiescedAtClose atomic.Bool
	machine := &fakeMachine{active: true}
	service, _ := teardownTestServer(t, machine)
	machine.onClose = func() { quiescedAtClose.Store(service.balloonQuiesced()) }
	service.vmStarts = map[string]map[string]time.Time{}
	service.provisions = map[string]activeProvision{}
	if err := service.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if !quiescedAtClose.Load() {
		t.Fatal("VM closed while the balloon manager was still allowed to retarget")
	}
	if !service.balloonQuiesced() {
		t.Fatal("balloon manager re-enabled after Shutdown")
	}
}

// closeNodes takes the balloon latch for the whole teardown.
func TestCloseNodesQuiescesBalloonUntilDone(t *testing.T) {
	var quiescedAtClose atomic.Bool
	machine := &fakeMachine{active: true}
	service, item := teardownTestServer(t, machine)
	machine.onClose = func() { quiescedAtClose.Store(service.balloonQuiesced()) }
	if err := service.closeNodes(item.Name, service.vms[item.Name], []string{item.Nodes[0].Name}); err != nil {
		t.Fatal(err)
	}
	if !quiescedAtClose.Load() {
		t.Fatal("VM closed without the balloon latch held")
	}
	if service.balloonQuiesced() {
		t.Fatal("balloon latch leaked after closeNodes")
	}
}

// overlapMachine reports whether two of its Close calls ever overlapped.
type overlapMachine struct {
	fakeMachine
	inFlight      *atomic.Int32
	overlap       *atomic.Bool
	stopsInFlight *atomic.Int32
	stopOverlap   *atomic.Bool
	mu            sync.Mutex
}

func (m *overlapMachine) Close() error {
	if m.inFlight.Add(1) > 1 {
		m.overlap.Store(true)
	}
	time.Sleep(5 * time.Millisecond)
	m.inFlight.Add(-1)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fakeMachine.Close()
}

// Stop records whether guest shutdowns overlapped, which they must: the ACPI
// wait is ~20s per node and serializing it would multiply teardown time.
func (m *overlapMachine) Stop(ctx context.Context) error {
	if m.stopsInFlight.Add(1) > 1 {
		m.stopOverlap.Store(true)
	}
	time.Sleep(20 * time.Millisecond)
	m.stopsInFlight.Add(-1)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fakeMachine.Stop(ctx)
}

// On a host that serializes teardown, guest shutdown still fans out but the
// host-side Close runs one VM at a time (#513).
func TestCloseNodesSequentialWhenConfigured(t *testing.T) {
	previous := closeVMsSequentially
	closeVMsSequentially = true
	t.Cleanup(func() { closeVMsSequentially = previous })
	var inFlight, stopsInFlight atomic.Int32
	var overlap, stopOverlap atomic.Bool
	machines := []hypervisor.Machine{}
	for range 3 {
		machines = append(machines, &overlapMachine{fakeMachine: fakeMachine{active: true},
			inFlight: &inFlight, overlap: &overlap, stopsInFlight: &stopsInFlight, stopOverlap: &stopOverlap})
	}
	service, item := teardownTestServer(t, machines...)
	names := []string{}
	for _, node := range item.Nodes {
		names = append(names, node.Name)
	}
	if err := service.closeNodes(item.Name, service.vms[item.Name], names); err != nil {
		t.Fatal(err)
	}
	if overlap.Load() {
		t.Fatal("closeNodes closed VMs concurrently with sequential teardown configured")
	}
	if !stopOverlap.Load() {
		t.Fatal("closeNodes serialized guest shutdown; Stop must fan out even when Close is sequential")
	}
	for _, machine := range machines {
		if calls := machine.(*overlapMachine).calls; len(calls) != 2 || calls[0] != "stop" || calls[1] != "close" {
			t.Fatalf("machine calls = %v, want stop then close", calls)
		}
	}
	if err := closeVMs(machines); err != nil {
		t.Fatal(err)
	}
	if overlap.Load() {
		t.Fatal("closeVMs closed VMs concurrently with sequential teardown configured")
	}
}
