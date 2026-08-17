package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// seedStoppedCluster saves a one-node cluster and returns its directory.
func seedStoppedCluster(t *testing.T, name string) string {
	t.Helper()
	item, err := cluster.New(name, 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	dir, err := cluster.Dir(name)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestStatusReportsSuspendedClusterAndHintsResume pins #272: a suspended
// cluster read as plain "stopped", and its hint pointed at start — the one verb
// that throws the saved memory away.
func TestStatusReportsSuspendedClusterAndHintsResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := seedStoppedCluster(t, "napping")
	if err := os.WriteFile(filepath.Join(dir, "napping-cp-1"+saveStateSuffix), []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}

	service := &Server{vms: make(map[string]map[string]hypervisor.Machine)}
	statuses, err := service.status(mustRawJSON(t, statusArgs{Cluster: "napping"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("status returned %d clusters, want 1", len(statuses))
	}
	status := statuses[0]
	if !status.Suspended {
		t.Fatalf("status.Suspended = false for a cluster holding %s", saveStateSuffix)
	}
	joined := strings.Join(status.Hints, "\n")
	if !strings.Contains(joined, "tbx cluster resume napping") {
		t.Fatalf("hints = %q, want the resume hint", status.Hints)
	}
	if strings.Contains(joined, "tbx cluster start napping") {
		t.Fatalf("hints = %q, must not point a suspended cluster at start", status.Hints)
	}
}

// TestStatusStoppedWithoutSavedStateKeepsStartHint keeps the plain stopped path
// unchanged: no saved memory, no resume advice.
func TestStatusStoppedWithoutSavedStateKeepsStartHint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedStoppedCluster(t, "idle")

	service := &Server{vms: make(map[string]map[string]hypervisor.Machine)}
	statuses, err := service.status(mustRawJSON(t, statusArgs{Cluster: "idle"}))
	if err != nil {
		t.Fatal(err)
	}
	status := statuses[0]
	if status.Suspended {
		t.Fatal("status.Suspended = true without saved state")
	}
	joined := strings.Join(status.Hints, "\n")
	if !strings.Contains(joined, "cluster is stopped — start it with: tbx cluster start idle") {
		t.Fatalf("hints = %q, want the stopped hint", status.Hints)
	}
}
