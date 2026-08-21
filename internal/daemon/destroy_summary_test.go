package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

func decodeDestroySummary(t *testing.T, response Response) DestroySummary {
	t.Helper()
	var summary DestroySummary
	if err := json.Unmarshal(response.Data, &summary); err != nil {
		t.Fatalf("decode DestroySummary: %v", err)
	}
	return summary
}

// destroy is the most destructive verb in the CLI and used to answer with a
// single line, leaving the operator nothing to check the destruction against
// (#422).
func TestDestroySummarisesWhatItRemoved(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range item.Nodes {
		if err := os.WriteFile(filepath.Join(dir, node.Name+".img"), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSnapshotDir(t, item.Name, "baseline")
	writeSnapshotDir(t, item.Name, "before-upgrade")

	raw, err := json.Marshal(destroyArgs{Name: item.Name, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatch(Request{Op: "cluster.destroy", Args: raw})
	if !response.OK {
		t.Fatalf("cluster.destroy failed: %s", response.Error)
	}

	summary := decodeDestroySummary(t, response)
	if summary.Name != item.Name {
		t.Fatalf("summary name = %q, want %q", summary.Name, item.Name)
	}
	if summary.Nodes == nil || *summary.Nodes != len(item.Nodes) {
		t.Fatalf("summary nodes = %v, want %d", summary.Nodes, len(item.Nodes))
	}
	if summary.Snapshots != 2 {
		t.Fatalf("summary snapshots = %d, want 2", summary.Snapshots)
	}
	if summary.DiskBytes < int64(len(item.Nodes)*4096) {
		t.Fatalf("summary diskBytes = %d, want at least the node disks", summary.DiskBytes)
	}
	if summary.Domain != item.EffectiveDomain() {
		t.Fatalf("summary domain = %q, want %q", summary.Domain, item.EffectiveDomain())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cluster directory survived the destroy: %v", err)
	}
}

// A cluster whose state file is already gone is still removable, and its
// summary reports what could still be counted rather than failing (#422).
func TestDestroySummaryOfAPartiallyDestroyedClusterCountsWhatIsLeft(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	delete(service.vms, item.Name)
	for _, node := range item.Nodes {
		if err := os.WriteFile(filepath.Join(dir, node.Name+".img"), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "cluster.json")); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(destroyArgs{Name: item.Name, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatch(Request{Op: "cluster.destroy", Args: raw})
	if !response.OK {
		t.Fatalf("cluster.destroy failed: %s", response.Error)
	}

	summary := decodeDestroySummary(t, response)
	// The disks still went, so an uncountable node count is reported as
	// unknown rather than as zero nodes removed.
	if summary.Name != item.Name || summary.Nodes != nil {
		t.Fatalf("summary = %+v, want the name and an uncounted node total", summary)
	}
	if summary.DiskBytes < 4096 {
		t.Fatalf("summary diskBytes = %d, want the state that was removed", summary.DiskBytes)
	}
}
