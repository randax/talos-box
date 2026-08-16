package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
	"github.com/randax/talos-box/internal/provision"
)

func runningStorageVMs(name string) map[string]map[string]hypervisor.Machine {
	return map[string]map[string]hypervisor.Machine{name: {"cp": &fakeMachine{active: true}}}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRefreshKubernetesReadinessDoesNotClaimReadyWithoutCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	statuses := []ClusterStatus{{
		Running: true,
		Name:    "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
		Nodes: []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}},
	}}
	refreshKubernetesReadiness(statuses)
	if statuses[0].KubernetesReady {
		t.Fatal("status claimed Kubernetes Ready without a valid kubeconfig")
	}
	for _, hint := range statuses[0].Hints {
		if strings.Contains(hint, "Kubernetes is Ready") {
			t.Fatalf("status emitted an unverified Ready hint: %q", hint)
		}
	}
}

func TestRefreshKubernetesReadinessSkipsStoppedFlannelClusters(t *testing.T) {
	statuses := []ClusterStatus{{
		Name:               "demo",
		Running:            false,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		KubernetesReady:    true,
		VIP:                "172.30.0.200",
		VIPLive:            true,
		Nodes:              []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseStopped, IP: "172.30.0.2"}},
	}}

	refreshKubernetesReadiness(statuses)

	if statuses[0].KubernetesReady {
		t.Fatal("stopped cluster reported Kubernetes ready")
	}
	if statuses[0].VIP != "" || statuses[0].VIPLive {
		t.Fatalf("stopped cluster retained stale VIP state: %+v", statuses[0])
	}
}

func TestProvisioningCompleteUsesMultiSecondProbeBudgets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secrets.yaml", "talosconfig", "kubeconfig"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	originalReady := kubernetesReadyProbe
	originalCilium := ciliumConvergenceProbe
	t.Cleanup(func() {
		kubernetesReadyProbe = originalReady
		ciliumConvergenceProbe = originalCilium
	})
	remaining := func(ctx context.Context) time.Duration {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("probe context has no deadline")
		}
		return time.Until(deadline)
	}
	kubernetesReadyProbe = func(ctx context.Context, _ []byte, _ []string) error {
		if got := remaining(ctx); got < 4*time.Second {
			t.Errorf("Kubernetes Ready budget = %s, want at least 4s", got)
		}
		return nil
	}
	ciliumConvergenceProbe = func(ctx context.Context, _ []byte, _ cluster.Cluster) error {
		if got := remaining(ctx); got < 14*time.Second {
			t.Errorf("Cilium convergence budget = %s, want at least 14s", got)
		}
		return nil
	}

	item := cluster.Cluster{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}}
	if !(&Server{}).provisioningComplete(item) {
		t.Fatal("healthy Cilium cluster was not considered complete")
	}
}

func TestProvisioningCompleteCiliumRequiresLiveVIPAndMatchingBGP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secrets.yaml", "talosconfig", "kubeconfig"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a kubeconfig"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	originalReady := kubernetesReadyProbe
	originalCilium := ciliumConvergenceProbe
	originalVIP := loadBalancerVIPProbe
	originalHelper := connectBGPHelper
	t.Cleanup(func() {
		kubernetesReadyProbe = originalReady
		ciliumConvergenceProbe = originalCilium
		loadBalancerVIPProbe = originalVIP
		connectBGPHelper = originalHelper
	})
	kubernetesReadyProbe = func(context.Context, []byte, []string) error { return nil }
	ciliumConvergenceProbe = func(context.Context, []byte, cluster.Cluster) error { return nil }

	tests := []struct {
		name      string
		vipLive   bool
		wantBGP   bool
		activeBGP bool
		want      bool
	}{
		{name: "VIP not live", want: false},
		{name: "VIP live and BGP disabled", vipLive: true, want: true},
		{name: "VIP live but stale BGP active", vipLive: true, activeBGP: true, want: false},
		{name: "VIP live but BGP inactive", vipLive: true, wantBGP: true, want: false},
		{name: "VIP live and BGP matches", vipLive: true, wantBGP: true, activeBGP: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loadBalancerVIPProbe = func(cluster.Cluster) (string, bool) { return "172.30.0.200", tt.vipLive }
			connectBGPHelper = func() (bgpHelperClient, error) {
				return &fakeBGPClient{active: tt.activeBGP}, nil
			}
			item := cluster.Cluster{
				Name: "demo",
				ProvisioningIntent: cluster.ProvisioningIntent{
					CNI: cluster.CNICilium,
					LB:  true,
					BGP: tt.wantBGP,
				},
			}
			if got := (&Server{}).provisioningComplete(item); got != tt.want {
				t.Fatalf("provisioningComplete() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestProvisionCNICiliumStorageDriftBypassesFastNoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secrets.yaml", "talosconfig", "kubeconfig"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	originalReady := kubernetesReadyProbe
	originalCilium := ciliumConvergenceProbe
	originalStorage := storageConvergenceProbe
	t.Cleanup(func() {
		kubernetesReadyProbe = originalReady
		ciliumConvergenceProbe = originalCilium
		storageConvergenceProbe = originalStorage
	})
	kubernetesReadyProbe = func(context.Context, []byte, []string) error { return nil }
	ciliumConvergenceProbe = func(context.Context, []byte, cluster.Cluster) error { return nil }
	storageConvergenceProbe = func(context.Context, cluster.Cluster, []byte) error {
		return errors.New("storage probe failed")
	}

	reconciled := false
	service := &Server{provisionReconcile: func(_ context.Context, request provision.Request) (provision.Result, error) {
		reconciled = true
		if _, ok := request.Storage.(provision.LonghornReconciler); !ok {
			t.Fatalf("storage reconciler = %T, want provision.LonghornReconciler", request.Storage)
		}
		return provision.Result{StoragePhase: provision.StoragePhaseProvisioning}, nil
	}}
	item := cluster.Cluster{
		Name: "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{
			CNI: cluster.CNICilium,
			CSI: cluster.CSILonghorn,
		},
	}
	_, phase, err := service.provisionCNI(context.Background(), item, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reconciled {
		t.Fatal("Cilium storage drift incorrectly took the fast no-op path")
	}
	if phase != StoragePhaseProvisioning {
		t.Fatalf("storage phase = %q, want %q while reconciliation resumes", phase, StoragePhaseProvisioning)
	}
}

func TestProvisioningCompleteEligibleRequiresObservedEndState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		intent      cluster.ProvisioningIntent
		credentials bool
		ready       bool
		vipLive     bool
		hubbleReady bool
		want        bool
	}{
		{name: "flannel without LoadBalancer", intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}, credentials: true, ready: true, want: true},
		{name: "flannel LoadBalancer VIP", intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}, credentials: true, ready: true, vipLive: true, want: true},
		{name: "missing credentials remints", intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}, ready: true},
		{name: "not Ready continues reconciliation", intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}, credentials: true},
		{name: "VIP unavailable continues reconciliation", intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}, credentials: true, ready: true},
		{name: "Cilium skips only after Hubble convergence", intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, Hubble: true}, credentials: true, ready: true, vipLive: true, hubbleReady: true, want: true},
		{name: "Cilium without LoadBalancer has no host BGP dependency", intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}, credentials: true, ready: true, hubbleReady: true, want: true},
		{name: "Cilium Hubble drift continues reconciliation", intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, Hubble: true}, credentials: true, ready: true, vipLive: true},
		{name: "BGP end state is complete before peer reassertion", intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}, credentials: true, ready: true, vipLive: true, hubbleReady: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := provisioningCompleteEligible(test.intent, test.credentials, test.ready, test.vipLive, test.hubbleReady); got != test.want {
				t.Fatalf("provisioningCompleteEligible(%+v, %t, %t, %t, %t) = %t, want %t", test.intent, test.credentials, test.ready, test.vipLive, test.hubbleReady, got, test.want)
			}
		})
	}
}

func TestRefreshStoragePhasesDefaultsCSIClustersToProvisioning(t *testing.T) {
	service := &Server{}
	for _, combination := range []struct {
		cni cluster.CNI
		csi cluster.CSI
	}{
		{cluster.CNIFlannel, cluster.CSILocalPath},
		{cluster.CNIFlannel, cluster.CSILonghorn},
		{cluster.CNICilium, cluster.CSILocalPath},
		{cluster.CNICilium, cluster.CSILonghorn},
	} {
		statuses := []ClusterStatus{{
			Name:               "demo",
			Running:            true,
			ProvisioningIntent: cluster.ProvisioningIntent{CNI: combination.cni, CSI: combination.csi},
		}}

		service.refreshStoragePhases(statuses)

		if statuses[0].StoragePhase != StoragePhaseProvisioning {
			t.Fatalf("cni=%s csi=%s storage phase = %q, want %q", combination.cni, combination.csi, statuses[0].StoragePhase, StoragePhaseProvisioning)
		}
	}
}

func TestProvisionCNIAttachesStorageReconcilerForCilium(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, test := range []struct {
		csi      cluster.CSI
		wantType string
	}{
		{cluster.CSILocalPath, "provision.LocalPathReconciler"},
		{cluster.CSILonghorn, "provision.LonghornReconciler"},
	} {
		item, err := cluster.New("demo-"+string(test.csi), 0, 1, 0, cluster.NodeDefaults{})
		if err != nil {
			t.Fatal(err)
		}
		item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: test.csi}
		service := &Server{provisionReconcile: func(_ context.Context, request provision.Request) (provision.Result, error) {
			if got := fmt.Sprintf("%T", request.Storage); got != test.wantType {
				t.Fatalf("csi=%s storage reconciler = %s, want %s", test.csi, got, test.wantType)
			}
			return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
		}}
		if _, phase, err := service.provisionCNI(context.Background(), item, true); err != nil {
			t.Fatal(err)
		} else if phase != StoragePhaseLive {
			t.Fatalf("csi=%s phase = %q, want %q", test.csi, phase, StoragePhaseLive)
		}
	}
}

func TestNodeAddReprovisionsRunningLonghornClusterWithUpdatedTopology(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	service.storagePhases[item.Name] = StoragePhaseLive

	var reconciledNodes int
	service.provisionReconcile = func(_ context.Context, request provision.Request) (provision.Result, error) {
		reconciledNodes = len(request.Cluster.Nodes)
		service.opMu.Lock()
		phase := service.storagePhases[request.Cluster.Name]
		service.opMu.Unlock()
		if phase != StoragePhaseProvisioning {
			t.Fatalf("storage phase during node.add reconcile = %q, want %q", phase, StoragePhaseProvisioning)
		}
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-1", Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatch(Request{Op: "node.add", Args: raw})
	if !response.OK {
		t.Fatalf("node.add failed: %s", response.Error)
	}
	if reconciledNodes != 2 {
		t.Fatalf("node.add reconciled %d nodes, want 2 after 1->2 Longhorn scale-up", reconciledNodes)
	}
	if service.storagePhases[item.Name] != StoragePhaseLive {
		t.Fatalf("storage phase after node.add = %q, want %q", service.storagePhases[item.Name], StoragePhaseLive)
	}
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Nodes) != 2 {
		t.Fatalf("cluster node count after node.add = %d, want 2", len(reloaded.Nodes))
	}
}

func TestNodeAddWarnsForLonghornCustomSchematic(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-1", Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.addNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Warning, "iscsi-tools") || !strings.Contains(result.Warning, "util-linux-tools") {
		t.Fatalf("NodeStatus.Warning = %q, want Longhorn custom schematic warning", result.Warning)
	}
}

func TestNodeAddSkipsLonghornCustomSchematicWarningForGeneratedDefault(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	service.defaultSchematic = item.Schematic

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-1", Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.addNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Warning, "iscsi-tools") || strings.Contains(result.Warning, "util-linux-tools") {
		t.Fatalf("NodeStatus.Warning = %q, did not want Longhorn custom schematic warning", result.Warning)
	}
}

func TestNodeRemoveReprovisionsRunningLonghornClusterWithUpdatedTopology(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	service.storagePhases[item.Name] = StoragePhaseLive

	var reconciledNodes int
	service.provisionReconcile = func(_ context.Context, request provision.Request) (provision.Result, error) {
		reconciledNodes = len(request.Cluster.Nodes)
		service.opMu.Lock()
		phase := service.storagePhases[request.Cluster.Name]
		service.opMu.Unlock()
		if phase != StoragePhaseProvisioning {
			t.Fatalf("storage phase during node.remove reconcile = %q, want %q", phase, StoragePhaseProvisioning)
		}
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatch(Request{Op: "node.remove", Args: raw})
	if !response.OK {
		t.Fatalf("node.remove failed: %s", response.Error)
	}
	if reconciledNodes != 2 {
		t.Fatalf("node.remove reconciled %d nodes, want 2 after Longhorn scale-down", reconciledNodes)
	}
	if service.storagePhases[item.Name] != StoragePhaseLive {
		t.Fatalf("storage phase after node.remove = %q, want %q", service.storagePhases[item.Name], StoragePhaseLive)
	}
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Nodes) != 2 {
		t.Fatalf("cluster node count after node.remove = %d, want 2", len(reloaded.Nodes))
	}
}

func TestRefreshStoragePhasesUsesStoredLiveObservation(t *testing.T) {
	service := &Server{storagePhases: map[string]StoragePhase{"demo": StoragePhaseLive}}
	statuses := []ClusterStatus{{
		Name:               "demo",
		Running:            true,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
	}}

	service.refreshStoragePhases(statuses)

	if statuses[0].StoragePhase != StoragePhaseLive {
		t.Fatalf("storage phase = %q, want %q", statuses[0].StoragePhase, StoragePhaseLive)
	}
}

func TestRefreshStoragePhasesReprobesAfterDaemonRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kubeconfig"), []byte("credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed := make(chan string, 1)
	service := &Server{vms: runningStorageVMs("demo"), storageProbe: func(_ context.Context, kubeconfig []byte) error {
		observed <- string(kubeconfig)
		return nil
	}}
	statuses := []ClusterStatus{{
		Name: "demo", Running: true, KubernetesReady: true,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
	}}

	service.refreshStoragePhases(statuses)
	if statuses[0].StoragePhase != StoragePhaseProvisioning {
		t.Fatalf("initial storage phase=%q, want non-blocking provisioning", statuses[0].StoragePhase)
	}
	if got := <-observed; got != "credentials" {
		t.Fatalf("kubeconfig = %q", got)
	}
	eventually(t, func() bool {
		service.opMu.Lock()
		defer service.opMu.Unlock()
		return service.storagePhases["demo"] == StoragePhaseLive
	})
	service.refreshStoragePhases(statuses)
	if statuses[0].StoragePhase != StoragePhaseLive {
		t.Fatalf("storage phase=%q after background probe, want live", statuses[0].StoragePhase)
	}
}

func TestRefreshStoragePhasesLeavesClustersWithoutCSIUnset(t *testing.T) {
	service := &Server{}
	statuses := []ClusterStatus{{
		Name: "demo", Running: true,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
	}}

	service.refreshStoragePhases(statuses)

	if statuses[0].StoragePhase != "" {
		t.Fatalf("storage phase = %q, want empty when CSI is absent", statuses[0].StoragePhase)
	}
}

func TestRefreshStoragePhasesSharesConcurrentRestartProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kubeconfig"), []byte("credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	service := &Server{vms: runningStorageVMs("demo"), storageProbe: func(context.Context, []byte) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}}
	newStatuses := func() []ClusterStatus {
		return []ClusterStatus{{
			Name: "demo", Running: true, KubernetesReady: true,
			ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
		}}
	}
	first, second := newStatuses(), newStatuses()
	service.refreshStoragePhases(first)
	<-entered
	service.refreshStoragePhases(second)
	close(release)
	eventually(t, func() bool {
		service.opMu.Lock()
		defer service.opMu.Unlock()
		return service.storagePhases["demo"] == StoragePhaseLive
	})
	if calls.Load() != 1 {
		t.Fatalf("storage probe calls = %d, want one shared observation", calls.Load())
	}
	if first[0].StoragePhase != StoragePhaseProvisioning || second[0].StoragePhase != StoragePhaseProvisioning {
		t.Fatalf("initial storage phases = %q, %q; want non-blocking provisioning", first[0].StoragePhase, second[0].StoragePhase)
	}
	third := newStatuses()
	service.refreshStoragePhases(third)
	if third[0].StoragePhase != StoragePhaseLive {
		t.Fatalf("storage phase after shared probe = %q, want live", third[0].StoragePhase)
	}
}

func TestRefreshStoragePhasesSkipsProbeUntilKubernetesIsReady(t *testing.T) {
	called := false
	service := &Server{storageProbe: func(context.Context, []byte) error {
		called = true
		return nil
	}}
	statuses := []ClusterStatus{{
		Name: "demo", Running: true, KubernetesReady: false,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
	}}
	service.refreshStoragePhases(statuses)
	if called {
		t.Fatal("storage probe ran while Kubernetes was not ready")
	}
	if statuses[0].StoragePhase != StoragePhaseProvisioning {
		t.Fatalf("storage phase = %q, want provisioning", statuses[0].StoragePhase)
	}
}

func TestStaleRunningSnapshotCannotStartProbeAfterStop(t *testing.T) {
	called := make(chan struct{}, 1)
	service := &Server{
		vms: map[string]map[string]hypervisor.Machine{},
		storageProbe: func(context.Context, []byte) error {
			called <- struct{}{}
			return nil
		},
	}
	statuses := []ClusterStatus{{
		Name: "demo", Running: true, KubernetesReady: true,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
	}}
	service.refreshStoragePhases(statuses)
	select {
	case <-called:
		t.Fatal("stale running snapshot started a probe after the VM lifetime ended")
	case <-time.After(10 * time.Millisecond):
	}
}

func TestCompletedProbeCannotPublishLiveAfterStop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kubeconfig"), []byte("credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	service := &Server{
		vms: runningStorageVMs("demo"),
		storageProbe: func(context.Context, []byte) error {
			close(entered)
			<-release
			return nil
		},
	}
	service.beginStorageStatusProbe("demo")
	<-entered
	service.opMu.Lock()
	service.vms = map[string]map[string]hypervisor.Machine{}
	service.invalidateStoragePhaseLocked("demo")
	service.opMu.Unlock()
	close(release)
	time.Sleep(10 * time.Millisecond)
	service.opMu.Lock()
	defer service.opMu.Unlock()
	if service.storagePhases["demo"] == StoragePhaseLive {
		t.Fatal("probe from stopped VM lifetime published storage live")
	}
}

func TestFailedBackgroundProbeBacksOffAndSurfacesReason(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := cluster.Dir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kubeconfig"), []byte("credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	service := &Server{
		vms: runningStorageVMs("demo"),
		storageProbe: func(context.Context, []byte) error {
			calls.Add(1)
			return errors.New("default StorageClass is not local-path")
		},
	}
	statuses := []ClusterStatus{{
		Name: "demo", Running: true, KubernetesReady: true,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
	}}
	service.refreshStoragePhases(statuses)
	eventually(t, func() bool {
		service.opMu.Lock()
		defer service.opMu.Unlock()
		return service.storageProbeFailures["demo"].message != ""
	})
	service.refreshStoragePhases(statuses)
	service.refreshStoragePhases(statuses)
	if calls.Load() != 1 {
		t.Fatalf("storage probe calls = %d, want one during backoff", calls.Load())
	}
	if !strings.Contains(statuses[0].StorageError, "default StorageClass is not local-path") {
		t.Fatalf("storage error = %q", statuses[0].StorageError)
	}
	if hints := strings.Join(statuses[0].Hints, "\n"); !strings.Contains(hints, "probe failed") || !strings.Contains(hints, "retrying after backoff") {
		t.Fatalf("storage hints = %q", hints)
	}
}

func runningLonghornClusterForNodeMutation(t *testing.T, controlPlanes, workers int) (*Server, cluster.Cluster) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	item, err := cluster.New("demo", 0, controlPlanes, workers, cluster.NodeDefaults{MemoryMiB: 1, DiskGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn}
	item.Schematic = "custom-schematic"
	item.TalosVersion = "v1.2.3"
	item.ImageArchitecture = string(hypervisor.ArchitectureARM64)
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}

	cacheRoot := filepath.Join(home, "cache")
	cachedDisk := filepath.Join(cacheRoot, item.Schematic, item.TalosVersion, string(hypervisor.ArchitectureARM64), "disk.raw")
	if err := os.MkdirAll(filepath.Dir(cachedDisk), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachedDisk, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	nodes := make(map[string]hypervisor.Machine, len(item.Nodes))
	for _, node := range item.Nodes {
		nodes[node.Name] = &fakeMachine{active: true}
	}

	return &Server{
		cache:                imagecache.New(cacheRoot),
		hypervisor:           &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
		vms:                  map[string]map[string]hypervisor.Machine{item.Name: nodes},
		provisions:           make(map[string]activeProvision),
		storagePhases:        make(map[string]StoragePhase),
		storageStatusProbes:  make(map[string]activeStorageProbe),
		storageProbeFailures: make(map[string]storageProbeFailure),
		hostPressure:         noHostPressure,
		subnetSources:        emptySubnetSources(),
		defaultSchematic:     "curated-default",
	}, item
}

func TestProvisionTimeoutGrowsWhenStorageIsDeclared(t *testing.T) {
	base := cluster.Cluster{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}}
	if got := provisionTimeout(base); got != cniProvisionTimeout {
		t.Fatalf("provisionTimeout(no csi) = %v, want %v", got, cniProvisionTimeout)
	}
	base.CSI = cluster.CSILonghorn
	if got := provisionTimeout(base); got != storageProvisionTimeout {
		t.Fatalf("provisionTimeout(csi) = %v, want %v", got, storageProvisionTimeout)
	}
	if storageProvisionTimeout <= cniProvisionTimeout {
		t.Fatalf("storageProvisionTimeout %v must exceed cniProvisionTimeout %v: a storage stage adds engine image pulls and the write/readback probe", storageProvisionTimeout, cniProvisionTimeout)
	}
}
