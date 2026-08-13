package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

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
	called := false
	service := &Server{storageProbe: func(_ context.Context, kubeconfig []byte) error {
		called = true
		if string(kubeconfig) != "credentials" {
			t.Fatalf("kubeconfig = %q", kubeconfig)
		}
		return nil
	}}
	statuses := []ClusterStatus{{
		Name: "demo", Running: true,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
	}}

	service.refreshStoragePhases(statuses)

	if !called || statuses[0].StoragePhase != StoragePhaseLive {
		t.Fatalf("called=%v storage phase=%q, want fresh live observation", called, statuses[0].StoragePhase)
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
			Name: "demo", Running: true,
			ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
		}}
	}
	first, second := newStatuses(), newStatuses()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); service.refreshStoragePhases(first) }()
	<-entered
	go func() { defer wg.Done(); service.refreshStoragePhases(second) }()
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("storage probe calls = %d, want one shared observation", calls.Load())
	}
	if first[0].StoragePhase != StoragePhaseLive || second[0].StoragePhase != StoragePhaseLive {
		t.Fatalf("storage phases = %q, %q; want live", first[0].StoragePhase, second[0].StoragePhase)
	}
}
