package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// stubNodeService replaces the live machine-API probe for one test. Nothing in
// the package may dial a node, so every test that exercises the service surface
// installs its own reading.
func stubNodeServices(t *testing.T, probe func(clusterName, ip string, now time.Time) ([]NodeService, ServiceProbe)) {
	t.Helper()
	previous := probeNodeServices
	probeNodeServices = probe
	t.Cleanup(func() { probeNodeServices = previous })
}

// TestClassifyService pins the rules that turn one machine-API observation into
// the word status prints (#357).
func TestClassifyService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		observation ServiceObservation
		want        ServiceHealth
	}{
		{
			name:        "no state at all is unknown",
			observation: ServiceObservation{HealthUnknown: true},
			want:        ServiceHealthUnknown,
		},
		{
			name:        "running and healthy",
			observation: ServiceObservation{State: "Running", Healthy: true},
			want:        ServiceHealthHealthy,
		},
		{
			name:        "running and healthy despite earlier failures",
			observation: ServiceObservation{State: "Running", Healthy: true, Failures: 5},
			want:        ServiceHealthHealthy,
		},
		{
			name:        "failed state is a crash loop",
			observation: ServiceObservation{State: "Failed", HealthUnknown: true},
			want:        ServiceHealthCrashLooping,
		},
		{
			name:        "repeated failure events are a crash loop even mid-restart",
			observation: ServiceObservation{State: "Preparing", HealthUnknown: true, Failures: crashLoopFailures},
			want:        ServiceHealthCrashLooping,
		},
		{
			name:        "a single restart is still starting",
			observation: ServiceObservation{State: "Preparing", HealthUnknown: true, Failures: 1},
			want:        ServiceHealthStarting,
		},
		{
			name:        "running without a verdict is unknown",
			observation: ServiceObservation{State: "Running", HealthUnknown: true},
			want:        ServiceHealthUnknown,
		},
		{
			name:        "running with a failing health probe is unhealthy",
			observation: ServiceObservation{State: "Running"},
			want:        ServiceHealthUnhealthy,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyService(kubeletService, tt.observation)
			if got.Health != tt.want {
				t.Fatalf("classifyService(%+v).Health = %q, want %q", tt.observation, got.Health, tt.want)
			}
			if got.Name != kubeletService || got.State != tt.observation.State {
				t.Fatalf("classifyService lost the observation: %+v", got)
			}
		})
	}
}

// TestClassifyServiceKeepsTheDiagnosisShort keeps one node's message from
// taking over the hint it lands in.
func TestClassifyServiceKeepsTheDiagnosisShort(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", serviceMessageMaxLen*2)
	got := classifyService(kubeletService, ServiceObservation{State: "Failed", Message: long})
	if len(got.Message) > serviceMessageMaxLen+len("…") {
		t.Fatalf("message not truncated: %d chars", len(got.Message))
	}
	if !strings.HasSuffix(got.Message, "…") {
		t.Fatalf("truncated message does not say so: %q", got.Message)
	}
}

// TestRefreshNodeStatusesProbesConfiguredNodesOnly keeps the probe
// proportionate: a stopped or unreachable node has no machine API to ask, and
// the stall sweep's statuses (Running unset) must stay probe-free.
func TestRefreshNodeStatusesProbesConfiguredNodesOnly(t *testing.T) {
	asked := make(chan string, 8)
	stubNodeServices(t, func(_, ip string, _ time.Time) ([]NodeService, ServiceProbe) {
		asked <- ip
		return []NodeService{classifyService(kubeletService, ServiceObservation{State: "Running", Healthy: true})}, ServiceProbe{Status: ServiceProbeSucceeded}
	})

	service := &Server{
		vms:          map[string]map[string]hypervisor.Machine{},
		nodeIPLookup: func(mac string, _ int) string { return "172.30.0." + mac },
		nodeProbe: func(ip string) ProbeResult {
			return ProbeResult{Dialed: ip == "172.30.0.2", TLS: ip == "172.30.0.2"}
		},
	}
	statuses := []ClusterStatus{{
		Name:    "demo",
		Running: true,
		Nodes: []NodeStatus{
			{Name: "demo-cp-1", Role: cluster.RoleControlPlane, MAC: "2", Phase: PhaseConfigured},
			{Name: "demo-worker-1", Role: cluster.RoleWorker, MAC: "3", Phase: PhaseConfigured},
		},
	}}
	service.refreshNodeStatuses(statuses)
	close(asked)

	var probed []string
	for entry := range asked {
		probed = append(probed, entry)
	}
	if len(probed) != 1 || probed[0] != "172.30.0.2" {
		t.Fatalf("kubelet probes = %v, want only the configured node", probed)
	}
	if statuses[0].Nodes[0].Kubelet == nil || statuses[0].Nodes[0].Kubelet.Health != ServiceHealthHealthy {
		t.Fatalf("configured node carries no kubelet reading: %+v", statuses[0].Nodes[0])
	}
	if statuses[0].Nodes[1].Kubelet != nil {
		t.Fatalf("unreachable node reports a kubelet reading: %+v", statuses[0].Nodes[1].Kubelet)
	}
}

// TestRefreshNodeServicesSkipsAStoppedCluster pins the gate the periodic stall
// sweep relies on: its synthetic statuses leave Running unset, and a sweep that
// dialled every node's machine API every tick would not be proportionate.
func TestRefreshNodeServicesSkipsAStoppedCluster(t *testing.T) {
	stubNodeServices(t, func(string, string, time.Time) ([]NodeService, ServiceProbe) {
		t.Fatal("probed a cluster that is not running")
		return nil, ServiceProbe{}
	})
	status := ClusterStatus{Name: "demo", Nodes: []NodeStatus{{Name: "demo-cp-1", IP: "172.30.0.2", Phase: PhaseConfigured}}}
	refreshNodeServices(&status, time.Now())
}

// TestRefreshNodeServicesSurfacesAndClassifiesTheWholeList pins the one-list
// status contract: kubelet remains a compatibility projection, while every
// service and every proven startup stall stays visible to newer consumers.
func TestRefreshNodeServicesSurfacesAndClassifiesTheWholeList(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	oldest := now.Add(-8 * time.Minute)
	newer := now.Add(-4 * time.Minute)
	started := now.Add(-10 * time.Minute)
	stubNodeServices(t, func(clusterName, ip string, gotNow time.Time) ([]NodeService, ServiceProbe) {
		if clusterName != "demo" || ip != "172.30.0.2" || !gotNow.Equal(now) {
			t.Fatalf("probe arguments = %q %q %s", clusterName, ip, gotNow)
		}
		return []NodeService{
			{Name: "kubelet", State: "Running", Health: ServiceHealthHealthy},
			{Name: "etcd", State: "Starting", Health: ServiceHealthStarting, Since: &newer},
			{Name: "apid", State: "Preparing", Health: ServiceHealthStarting, Since: &oldest},
		}, ServiceProbe{Status: ServiceProbeSucceeded, Source: "/tmp/talosconfig"}
	})
	status := ClusterStatus{Name: "demo", Running: true, Nodes: []NodeStatus{{
		Name: "demo-cp-1", IP: "172.30.0.2", Phase: PhaseConfigured, StartedAt: &started,
	}}}

	refreshNodeServices(&status, now)

	node := status.Nodes[0]
	if got := []string{node.Services[0].Name, node.Services[1].Name, node.Services[2].Name}; !reflect.DeepEqual(got, []string{"apid", "etcd", "kubelet"}) {
		t.Fatalf("service order = %v", got)
	}
	if node.Kubelet == nil || node.Kubelet.Health != ServiceHealthHealthy {
		t.Fatalf("kubelet compatibility projection = %+v", node.Kubelet)
	}
	wantStalls := []StalledService{{Service: "apid", State: "Preparing", Since: oldest}, {Service: "etcd", State: "Starting", Since: newer}}
	if !reflect.DeepEqual(node.StalledServices, wantStalls) {
		t.Fatalf("stalled services = %+v, want %+v", node.StalledServices, wantStalls)
	}
	if node.ServiceProbe.Status != ServiceProbeSucceeded || node.ServiceProbe.Source != "/tmp/talosconfig" {
		t.Fatalf("service probe = %+v", node.ServiceProbe)
	}
}

func TestServiceStallsRequireAUsableOldTimestamp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	exactly := now.Add(-serviceStallThreshold)
	young := exactly.Add(time.Second)
	future := now.Add(time.Second)
	started := now.Add(-time.Minute)
	beforeRestart := now.Add(-time.Hour)
	tests := []struct {
		name    string
		service NodeService
		start   *time.Time
		want    bool
	}{
		{name: "older preparing", service: NodeService{Name: "kubelet", State: "Preparing", Since: timePointer(now.Add(-serviceStallThreshold - time.Second))}, want: true},
		{name: "older starting case insensitive", service: NodeService{Name: "etcd", State: "sTaRtInG", Since: timePointer(now.Add(-serviceStallThreshold - time.Second))}, want: true},
		{name: "exactly threshold", service: NodeService{Name: "kubelet", State: "Preparing", Since: &exactly}},
		{name: "younger", service: NodeService{Name: "kubelet", State: "Starting", Since: &young}},
		{name: "running", service: NodeService{Name: "kubelet", State: "Running", Since: timePointer(now.Add(-time.Hour))}},
		{name: "missing", service: NodeService{Name: "kubelet", State: "Preparing"}},
		{name: "future", service: NodeService{Name: "kubelet", State: "Preparing", Since: &future}},
		{name: "retained before restart clamps", service: NodeService{Name: "kubelet", State: "Preparing", Since: &beforeRestart}, start: &started},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stalledServices([]NodeService{tt.service}, tt.start, now)
			if (len(got) != 0) != tt.want {
				t.Fatalf("stalledServices() = %+v, want stalled=%v", got, tt.want)
			}
		})
	}
}

func TestObserveServiceUsesLatestMatchingValidEvent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	older := now.Add(-9 * time.Minute)
	latest := now.Add(-4 * time.Minute)
	observation := observeServiceAt(serviceInfo("kubelet", "Preparing",
		serviceEvent("Preparing", older),
		serviceEvent("Running", now.Add(-5*time.Minute)),
		serviceEvent("Preparing", latest),
		serviceEvent("Preparing", now.Add(time.Minute)),
	), now)
	if observation.Since == nil || !observation.Since.Equal(latest) {
		t.Fatalf("since = %v, want %s", observation.Since, latest)
	}
}

func TestLookupNodeTalosContextUsesOnlyTheExactContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TALOS_HOME", "")
	shared := filepath.Join(home, "shared-talosconfig")
	t.Setenv("TALOSCONFIG", shared)
	writeTalosContextFile(t, shared, "other", map[string]string{"other": "shared-ca", "demo-1": "renamed-ca"})
	if _, _, err := lookupNodeTalosContextLive("demo"); !errors.Is(err, errTalosContextMissing) {
		t.Fatalf("lookup with unrelated current context = %v, want missing context", err)
	}

	writeTalosContextFile(t, shared, "other", map[string]string{"other": "shared-ca", "demo": "exact-ca"})
	source, ctx, err := lookupNodeTalosContextLive("demo")
	if err != nil || source != shared || ctx.CA != "exact-ca" {
		t.Fatalf("shared lookup = source %q context %+v error %v", source, ctx, err)
	}

	clusterDir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(clusterDir, "talosconfig")
	writeTalosContextFile(t, local, "other", map[string]string{"demo": "local-ca"})
	source, ctx, err = lookupNodeTalosContextLive("demo")
	if err != nil || source != local || ctx.CA != "local-ca" {
		t.Fatalf("cluster-local lookup = source %q context %+v error %v", source, ctx, err)
	}
}

func TestLookupNodeTalosContextUsesTalosHomeWithoutCreatingFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TALOSCONFIG", "")
	talosHome := filepath.Join(home, "custom-talos-home")
	t.Setenv("TALOS_HOME", talosHome)
	path := filepath.Join(talosHome, "config")
	writeTalosContextFile(t, path, "other", map[string]string{"demo": "home-ca", "other": "other-ca"})

	source, ctx, err := lookupNodeTalosContextLive("demo")
	if err != nil || source != path || ctx.CA != "home-ca" {
		t.Fatalf("TALOS_HOME lookup = source %q context %+v error %v", source, ctx, err)
	}
}

func TestLookupNodeTalosContextUsesTheDefaultUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TALOSCONFIG", "")
	t.Setenv("TALOS_HOME", "")
	path := filepath.Join(home, ".talos", "config")
	writeTalosContextFile(t, path, "other", map[string]string{"demo": "default-ca", "other": "other-ca"})

	source, ctx, err := lookupNodeTalosContextLive("demo")
	if err != nil || source != path || ctx.CA != "default-ca" {
		t.Fatalf("default user lookup = source %q context %+v error %v", source, ctx, err)
	}
}

func TestLookupNodeTalosContextKeepsSearchingPastAFileWithoutTheContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TALOS_HOME", "")
	first := filepath.Join(home, "first-talosconfig")
	t.Setenv("TALOSCONFIG", first)
	// The first default path is a valid talosconfig that simply lacks the
	// cluster's context; the exact context lives in the next candidate.
	writeTalosContextFile(t, first, "other", map[string]string{"other": "first-ca"})
	second := filepath.Join(home, ".talos", "config")
	writeTalosContextFile(t, second, "other", map[string]string{"demo": "second-ca", "other": "other-ca"})

	source, ctx, err := lookupNodeTalosContextLive("demo")
	if err != nil || source != second || ctx.CA != "second-ca" {
		t.Fatalf("lookup past a context-less file = source %q context %+v error %v, want %s", source, ctx, err, second)
	}

	// A malformed candidate is still an error, not a file to skip past.
	if err := os.WriteFile(first, []byte("context: [broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lookupNodeTalosContextLive("demo"); err == nil || errors.Is(err, errTalosContextMissing) {
		t.Fatalf("lookup with a malformed first candidate = %v, want a parse error", err)
	}
}

func TestProbeNodeServicesUsesExactContextCredentialsAtTheObservedIP(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	exact := &clientconfig.Context{Endpoints: []string{"192.0.2.99"}, CA: "exact-ca", Crt: "exact-crt", Key: "exact-key"}
	previousLookup := lookupNodeTalosContext
	previousList := listNodeServices
	lookupNodeTalosContext = func(name string) (string, *clientconfig.Context, error) {
		if name != "demo" {
			t.Fatalf("context lookup = %q", name)
		}
		return "/tmp/shared-talosconfig", exact, nil
	}
	listNodeServices = func(_ context.Context, gotContext *clientconfig.Context, ip string) (*machineapi.ServiceListResponse, error) {
		if gotContext != exact || gotContext.CA != "exact-ca" || gotContext.Crt != "exact-crt" || gotContext.Key != "exact-key" {
			t.Fatalf("selected context = %+v, want exact demo credentials", gotContext)
		}
		if ip != "172.30.0.23" {
			t.Fatalf("service endpoint = %q, want observed lease", ip)
		}
		return &machineapi.ServiceListResponse{Messages: []*machineapi.ServiceList{{Services: []*machineapi.ServiceInfo{serviceInfo("kubelet", "Running")}}}}, nil
	}
	t.Cleanup(func() {
		lookupNodeTalosContext = previousLookup
		listNodeServices = previousList
	})

	services, probe := probeNodeServicesLive("demo", "172.30.0.23", now)
	if probe.Status != ServiceProbeSucceeded || probe.Source != "/tmp/shared-talosconfig" || len(services) != 1 {
		t.Fatalf("probe = %+v, services = %+v", probe, services)
	}
}

func TestLookupNodeTalosContextDoesNotHideAnInvalidClusterConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shared := filepath.Join(home, "shared-talosconfig")
	t.Setenv("TALOSCONFIG", shared)
	writeTalosContextFile(t, shared, "demo", map[string]string{"demo": "shared-ca"})
	clusterDir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(clusterDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clusterDir, "talosconfig"), []byte("not: [valid"), 0o600); err != nil {
		t.Fatal(err)
	}

	if source, ctx, err := lookupNodeTalosContextLive("demo"); err == nil || errors.Is(err, errTalosContextMissing) {
		t.Fatalf("invalid local config fell through: source %q context %+v error %v", source, ctx, err)
	}
}

func TestRefreshNodeServicesKeepsMissingCredentialsDistinctFromProbeFailure(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	stubNodeServices(t, func(_ string, ip string, _ time.Time) ([]NodeService, ServiceProbe) {
		if strings.HasSuffix(ip, ".2") {
			return nil, ServiceProbe{Status: ServiceProbeMissingCredentials}
		}
		return nil, ServiceProbe{Status: ServiceProbeFailed, Error: "rpc unavailable"}
	})
	status := ClusterStatus{Name: "demo", Running: true, Nodes: []NodeStatus{
		{Name: "demo-cp-1", IP: "172.30.0.2", Phase: PhaseConfigured},
		{Name: "demo-worker-1", IP: "172.30.0.3", Phase: PhaseConfigured},
	}}
	refreshNodeServices(&status, now)
	if status.Nodes[0].ServiceProbe.Status != ServiceProbeMissingCredentials {
		t.Fatalf("missing credentials = %+v", status.Nodes[0].ServiceProbe)
	}
	if status.Nodes[1].ServiceProbe.Status != ServiceProbeFailed || status.Nodes[1].ServiceProbe.Error != "rpc unavailable" {
		t.Fatalf("probe failure = %+v", status.Nodes[1].ServiceProbe)
	}
}

// TestRefreshNodeStatusesKeepsSuspendedNodes pins #360: the refresh re-derives
// the live phase, but the suspension it inherits is a disk fact — dropping it
// made a suspended cluster read as plain stopped in the PHASE column.
func TestRefreshNodeStatusesKeepsSuspendedNodes(t *testing.T) {
	service := &Server{
		vms:          map[string]map[string]hypervisor.Machine{},
		nodeIPLookup: func(string, int) string { return "" },
		nodeProbe:    func(string) ProbeResult { return ProbeResult{} },
	}
	statuses := []ClusterStatus{{
		Name:      "napping",
		Suspended: true,
		Nodes: []NodeStatus{
			{Name: "napping-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseStopped, Suspended: true},
			{Name: "napping-worker-1", Role: cluster.RoleWorker, Phase: PhaseStopped},
		},
	}}
	service.refreshNodeStatuses(statuses)

	if !statuses[0].Nodes[0].Suspended {
		t.Fatal("the node holding saved memory lost its suspended flag in the refresh")
	}
	if statuses[0].Nodes[1].Suspended {
		t.Fatal("a node without saved memory gained a suspended flag")
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func serviceEvent(state string, timestamp time.Time) *machineapi.ServiceEvent {
	return &machineapi.ServiceEvent{State: state, Ts: timestamppb.New(timestamp)}
}

func serviceInfo(name, state string, events ...*machineapi.ServiceEvent) *machineapi.ServiceInfo {
	return &machineapi.ServiceInfo{Id: name, State: state, Events: &machineapi.ServiceEvents{Events: events}}
}

func writeTalosContextFile(t *testing.T, path, current string, contexts map[string]string) {
	t.Helper()
	contents := "context: " + current + "\ncontexts:\n"
	for name, ca := range contexts {
		contents += "  " + name + ":\n    endpoints: [192.0.2.10]\n    ca: " + ca + "\n    crt: " + name + "-crt\n    key: " + name + "-key\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
