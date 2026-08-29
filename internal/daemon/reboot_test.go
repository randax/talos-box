package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

func TestRebootLogObservations(t *testing.T) {
	var log rebootLog
	now := time.Date(2026, 8, 29, 7, 0, 0, 0, time.UTC)
	if _, changed := log.observe("demo/cp-1", 100, now); changed {
		t.Fatal("first observation classified as reboot")
	}
	if _, changed := log.observe("demo/cp-1", 100, now.Add(time.Second)); changed {
		t.Fatal("same boot time classified as reboot")
	}
	observation, changed := log.observe("demo/cp-1", 200, now.Add(2*time.Second))
	if !changed || observation.BootTime != 200 || !observation.RebootedAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("first change = %+v, %v", observation, changed)
	}
	if _, changed := log.observe("demo/cp-1", 200, now.Add(3*time.Second)); changed {
		t.Fatal("new baseline relogged")
	}
	if _, changed := log.observe("demo/cp-1", 300, now.Add(4*time.Second)); !changed {
		t.Fatal("second boot-time change was not reported")
	}
}

func TestRebootLogIgnoresZeroAndExpiresNotice(t *testing.T) {
	var log rebootLog
	now := time.Date(2026, 8, 29, 7, 0, 0, 0, time.UTC)
	if _, changed := log.observe("demo/cp-1", 0, now); changed {
		t.Fatal("zero boot time classified as reboot")
	}
	log.observe("demo/cp-1", 100, now)
	log.observe("demo/cp-1", 200, now.Add(time.Second))
	if _, ok := log.current("demo/cp-1", now.Add(rebootNoticeTTL-time.Nanosecond)); !ok {
		t.Fatal("notice expired before TTL")
	}
	if _, ok := log.current("demo/cp-1", now.Add(time.Second+rebootNoticeTTL)); ok {
		t.Fatal("notice remained active at TTL boundary")
	}
}

func TestRebootTrackingForgetPaths(t *testing.T) {
	server := &Server{}
	now := time.Now()
	for _, key := range []string{"a/one", "a/two", "b/one"} {
		server.reboots.observe(key, 1, now)
	}
	server.forgetNode("a", "one")
	if _, ok := server.reboots.current("a/one", now); ok || rebootKnown(&server.reboots, "a/one") {
		t.Fatal("forgetNode retained reboot baseline")
	}
	server.forgetCluster("a")
	if rebootKnown(&server.reboots, "a/two") {
		t.Fatal("forgetCluster retained reboot baseline")
	}
	server.forgetAllNodeTracking()
	if rebootKnown(&server.reboots, "b/one") {
		t.Fatal("forgetAllNodeTracking retained reboot baseline")
	}
}

func TestRecordVMStartClearsRebootBaseline(t *testing.T) {
	server := &Server{}
	now := time.Now()
	server.reboots.observe("demo/cp-1", 100, now)
	server.recordVMStart("demo", "cp-1")
	if _, changed := server.reboots.observe("demo/cp-1", 200, now.Add(time.Second)); changed {
		t.Fatal("deliberate VM start made the next observation look like a reboot")
	}
}

func TestRefreshNodeRebootsClassifiesWithinTTL(t *testing.T) {
	original := probeNodeBootTime
	t.Cleanup(func() { probeNodeBootTime = original })
	var mu sync.Mutex
	bootTime := uint64(100)
	probeNodeBootTime = func(clusterName, ip string) (uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		return bootTime, nil
	}
	server := &Server{}
	now := time.Date(2026, 8, 29, 7, 0, 0, 0, time.UTC)
	statuses := []ClusterStatus{{Name: "demo", Running: true, Nodes: []NodeStatus{{Name: "cp-1", IP: "172.30.0.2", Phase: PhaseConfigured}}}}
	server.refreshNodeReboots(statuses, now)
	mu.Lock()
	bootTime = 200
	mu.Unlock()
	server.refreshNodeReboots(statuses, now.Add(time.Second))
	node := statuses[0].Nodes[0]
	if node.Phase != PhaseRebooted || node.RebootedAt == nil || !node.RebootedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("node = %+v, want active reboot notice", node)
	}
	statuses[0].Nodes[0].Phase = PhaseConfigured
	server.refreshNodeReboots(statuses, now.Add(time.Second+rebootNoticeTTL))
	if statuses[0].Nodes[0].Phase != PhaseConfigured || statuses[0].Nodes[0].RebootedAt != nil {
		t.Fatalf("expired node = %+v", statuses[0].Nodes[0])
	}
}

func TestRefreshNodeRebootsSkipsIneligibleAndFailedProbes(t *testing.T) {
	original := probeNodeBootTime
	t.Cleanup(func() { probeNodeBootTime = original })
	probes := 0
	probeNodeBootTime = func(string, string) (uint64, error) {
		probes++
		return 0, errors.New("credentials unavailable")
	}
	server := &Server{}
	statuses := []ClusterStatus{
		{Name: "stopped", Nodes: []NodeStatus{{Name: "cp-1", IP: "1", Phase: PhaseStopped}}},
		{Name: "maintenance", Running: true, Nodes: []NodeStatus{{Name: "cp-1", IP: "2", Phase: PhaseMaintenance}}},
		{Name: "unreachable", Running: true, Nodes: []NodeStatus{{Name: "cp-1", IP: "3", Phase: PhaseUnreachable}}},
		{Name: "no-ip", Running: true, Nodes: []NodeStatus{{Name: "cp-1", Phase: PhaseConfigured}}},
		{Name: "error", Running: true, Nodes: []NodeStatus{{Name: "cp-1", IP: "4", Phase: PhaseConfigured}}},
	}
	server.refreshNodeReboots(statuses, time.Now())
	if probes != 1 {
		t.Fatalf("probes = %d, want only eligible node", probes)
	}
	for _, status := range statuses {
		if status.Nodes[0].Phase == PhaseRebooted || status.Nodes[0].RebootedAt != nil {
			t.Fatalf("%s fabricated reboot: %+v", status.Name, status.Nodes[0])
		}
	}
}

func TestConcurrentRebootObservationEmitsOneChange(t *testing.T) {
	var log rebootLog
	now := time.Now()
	log.observe("demo/cp-1", 100, now)
	var wait sync.WaitGroup
	changes := make(chan bool, 32)
	for i := 0; i < cap(changes); i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, changed := log.observe("demo/cp-1", 200, now.Add(time.Second))
			changes <- changed
		}()
	}
	wait.Wait()
	close(changes)
	count := 0
	for changed := range changes {
		if changed {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("change events = %d, want 1", count)
	}
}

func TestProbeNodeBootTimeUsesTypedSystemStat(t *testing.T) {
	originalLookup, originalRead := lookupNodeTalosContext, readNodeSystemStat
	t.Cleanup(func() {
		lookupNodeTalosContext = originalLookup
		readNodeSystemStat = originalRead
	})
	lookupNodeTalosContext = func(clusterName string) (string, *clientconfig.Context, error) {
		if clusterName != "demo" {
			t.Fatalf("cluster = %q", clusterName)
		}
		return "/tmp/talosconfig", &clientconfig.Context{}, nil
	}
	readNodeSystemStat = func(ctx context.Context, _ *clientconfig.Context, ip string) (*machineapi.SystemStatResponse, error) {
		if ip != "172.30.0.2" {
			t.Fatalf("ip = %q", ip)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("system stat probe has no deadline")
		}
		return &machineapi.SystemStatResponse{Messages: []*machineapi.SystemStat{{BootTime: 12345}}}, nil
	}
	bootTime, err := probeNodeBootTimeLive("demo", "172.30.0.2")
	if err != nil || bootTime != 12345 {
		t.Fatalf("probeNodeBootTimeLive() = %d, %v", bootTime, err)
	}
}

func TestBackgroundRefreshPathDetectsReboot(t *testing.T) {
	original := probeNodeBootTime
	t.Cleanup(func() { probeNodeBootTime = original })
	bootTime := uint64(100)
	probeNodeBootTime = func(string, string) (uint64, error) { return bootTime, nil }
	server := &Server{
		nodeIPLookup: func(string, int) string { return "172.30.0.2" },
		nodeProbe:    func(string) ProbeResult { return ProbeResult{Dialed: true, TLS: true} },
	}
	statuses := []ClusterStatus{{
		Name: "demo", Running: true,
		Nodes: []NodeStatus{{Name: "demo-cp-1", Phase: PhaseConfigured}},
	}}
	server.refreshNodeStatuses(statuses)
	bootTime = 200
	server.refreshNodeStatuses(statuses)
	if statuses[0].Nodes[0].Phase != PhaseRebooted || statuses[0].Nodes[0].RebootedAt == nil {
		t.Fatalf("background refresh status = %+v", statuses[0].Nodes[0])
	}
}

func rebootKnown(log *rebootLog, key string) bool {
	log.mu.Lock()
	defer log.mu.Unlock()
	_, ok := log.nodes[key]
	return ok
}

func TestPhaseConfiguredIncludesRebooted(t *testing.T) {
	if !PhaseConfigured.Configured() || !PhaseRebooted.Configured() {
		t.Fatal("configured-like phases were not inspectable")
	}
	for _, phase := range []Phase{PhaseStopped, PhaseSuspended, PhaseUnreachable, PhaseMaintenance} {
		if phase.Configured() {
			t.Fatalf("phase %q classified as configured", phase)
		}
	}
}
