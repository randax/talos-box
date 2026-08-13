package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
)

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

func TestRefreshStoragePhasesDefaultsCSIClustersToProvisioning(t *testing.T) {
	service := &Server{}
	statuses := []ClusterStatus{{
		Name:               "demo",
		Running:            true,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
	}}

	service.refreshStoragePhases(statuses)

	if statuses[0].StoragePhase != StoragePhaseProvisioning {
		t.Fatalf("storage phase = %q, want %q", statuses[0].StoragePhase, StoragePhaseProvisioning)
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
	service := &Server{storageProbe: func(_ context.Context, kubeconfig []byte) error {
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

func TestRefreshStoragePhasesDoesNotClaimUnsupportedBackendIsProvisioning(t *testing.T) {
	service := &Server{}
	statuses := []ClusterStatus{{
		Name: "demo", Running: true,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn},
	}}

	service.refreshStoragePhases(statuses)

	if statuses[0].StoragePhase != "" {
		t.Fatalf("storage phase = %q, want unknown until Longhorn support is installed", statuses[0].StoragePhase)
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
	service := &Server{storageProbe: func(context.Context, []byte) error {
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
