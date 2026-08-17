package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// withTalosHome points cluster state at a short-path temp dir (macOS socket
// path limits don't apply here, but keep it consistent and isolated).
func withTalosHome(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tbxsnap")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
}

func writeDisk(t *testing.T, dir, node, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, node+".img"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readDisk(t *testing.T, dir, node string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, node+".img"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func makeCluster(t *testing.T) Cluster {
	t.Helper()
	item, err := New("demo", 0, 1, 1, NodeDefaults{MemoryMiB: 2048, CPUs: 2, DiskGiB: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(item); err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir("demo")
	for _, node := range item.Nodes {
		writeDisk(t, dir, node.Name, "original-"+node.Name)
	}
	return item
}

func TestSnapshotCreateRestoreCycle(t *testing.T) {
	withTalosHome(t)
	item := makeCluster(t)
	dir, _ := Dir("demo")

	if err := CreateSnapshot(item, "before"); err != nil {
		t.Fatal(err)
	}
	// mutate every live disk
	for _, node := range item.Nodes {
		writeDisk(t, dir, node.Name, "mutated-"+node.Name)
	}

	if err := RestoreSnapshot(item, "before"); err != nil {
		t.Fatal(err)
	}
	for _, node := range item.Nodes {
		if got := readDisk(t, dir, node.Name); got != "original-"+node.Name {
			t.Errorf("node %s = %q, want restored original", node.Name, got)
		}
	}
}

func TestSnapshotListAndDelete(t *testing.T) {
	withTalosHome(t)
	item := makeCluster(t)

	for _, name := range []string{"alpha", "beta"} {
		if err := CreateSnapshot(item, name); err != nil {
			t.Fatal(err)
		}
	}
	names, err := ListSnapshots("demo")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(names))
	for i, s := range names {
		got[i] = s.Name
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("list = %v, want [alpha beta]", got)
	}

	if err := DeleteSnapshot("demo", "alpha"); err != nil {
		t.Fatal(err)
	}
	names, _ = ListSnapshots("demo")
	if len(names) != 1 || names[0].Name != "beta" {
		t.Errorf("after delete = %v, want [beta]", names)
	}
}

func TestSnapshotNameRejectsTraversal(t *testing.T) {
	withTalosHome(t)
	item := makeCluster(t)
	for _, bad := range []string{"..", ".", "../evil", "a/b", ""} {
		if err := CreateSnapshot(item, bad); err == nil {
			t.Errorf("CreateSnapshot(%q) should be rejected", bad)
		}
		if err := DeleteSnapshot("demo", bad); err == nil {
			t.Errorf("DeleteSnapshot(%q) should be rejected", bad)
		}
		if err := RestoreSnapshot(item, bad); err == nil {
			t.Errorf("RestoreSnapshot(%q) should be rejected", bad)
		}
	}
	// the cluster dir must still be intact after all those rejected calls
	if _, err := Load("demo"); err != nil {
		t.Fatalf("cluster dir damaged by rejected snapshot names: %v", err)
	}
}

func TestRestoreMissingSnapshotErrors(t *testing.T) {
	withTalosHome(t)
	item := makeCluster(t)
	if err := RestoreSnapshot(item, "nope"); err == nil {
		t.Fatal("restore of missing snapshot should error")
	}
}

func TestSnapshotSurvivesReload(t *testing.T) {
	withTalosHome(t)
	item := makeCluster(t)
	if err := CreateSnapshot(item, "keep"); err != nil {
		t.Fatal(err)
	}
	// simulate daemon restart: fresh Load, snapshot still listable and restorable
	reloaded, err := Load("demo")
	if err != nil {
		t.Fatal(err)
	}
	names, _ := ListSnapshots("demo")
	if len(names) != 1 {
		t.Fatalf("snapshot did not survive reload: %v", names)
	}
	if err := RestoreSnapshot(reloaded, "keep"); err != nil {
		t.Errorf("restore after reload failed: %v", err)
	}
}

func TestSnapshotNodesReportsTheCapturedNodeSet(t *testing.T) {
	withTalosHome(t)
	item := makeCluster(t)
	if err := CreateSnapshot(item, "before"); err != nil {
		t.Fatal(err)
	}

	nodes, err := SnapshotNodes("demo", "before")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(nodes))
	for i, node := range nodes {
		got[i] = node.Name
	}
	sort.Strings(got)
	want := make([]string, len(item.Nodes))
	for i, node := range item.Nodes {
		want[i] = node.Name
	}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("snapshot nodes = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("snapshot nodes = %v, want %v", got, want)
		}
	}
}

func TestSnapshotNodesRejectsMissingAndInvalidNames(t *testing.T) {
	withTalosHome(t)
	makeCluster(t)

	if _, err := SnapshotNodes("demo", "nope"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("SnapshotNodes of a missing snapshot = %v, want a does-not-exist error", err)
	}
	if _, err := SnapshotNodes("demo", "../evil"); err == nil {
		t.Fatal("SnapshotNodes accepted a traversing snapshot name")
	}
}

func TestRestoreSnapshotWithUnreadableStateLeavesTheClusterIntact(t *testing.T) {
	withTalosHome(t)
	item := makeCluster(t)
	dir, _ := Dir("demo")
	if err := CreateSnapshot(item, "before"); err != nil {
		t.Fatal(err)
	}
	snapshotState := filepath.Join(dir, "snapshots", "before", stateFile)
	for _, damage := range []func(){
		func() { _ = os.Remove(snapshotState) },
		func() { _ = os.WriteFile(snapshotState, []byte("{not json"), 0o600) },
	} {
		damage()
		for _, node := range item.Nodes {
			writeDisk(t, dir, node.Name, "live-"+node.Name)
		}

		if err := RestoreSnapshot(item, "before"); err == nil {
			t.Fatal("restore from a snapshot without readable state succeeded")
		}

		// the restore must fail before it deletes anything, or it destroys the
		// cluster it was asked to restore
		for _, node := range item.Nodes {
			if got := readDisk(t, dir, node.Name); got != "live-"+node.Name {
				t.Errorf("node %s = %q, want the untouched live disk", node.Name, got)
			}
		}
		if _, err := Load("demo"); err != nil {
			t.Fatalf("live cluster state destroyed by a failed restore: %v", err)
		}
	}
}

func TestRestoreSnapshotRejectsStateAndDiskDisagreement(t *testing.T) {
	withTalosHome(t)
	item := makeCluster(t)
	dir, _ := Dir("demo")
	if err := CreateSnapshot(item, "before"); err != nil {
		t.Fatal(err)
	}
	captured := item.Nodes[0].Name

	for _, testCase := range []struct {
		name     string
		snapshot string
		damage   func(dir string)
		want     string
	}{
		{
			name:     "captured node without its disk image",
			snapshot: "missing-image",
			damage:   func(snapshot string) { _ = os.Remove(filepath.Join(snapshot, captured+".img")) },
			want:     "missing the disk image",
		},
		{
			name:     "disk image for no captured node",
			snapshot: "stray-image",
			damage:   func(snapshot string) { writeDisk(t, snapshot, "demo-worker-9", "stray") },
			want:     "for no captured node",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := CreateSnapshot(item, testCase.snapshot); err != nil {
				t.Fatal(err)
			}
			testCase.damage(filepath.Join(dir, "snapshots", testCase.snapshot))
			for _, node := range item.Nodes {
				writeDisk(t, dir, node.Name, "live-"+node.Name)
			}

			err := RestoreSnapshot(item, testCase.snapshot)

			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("restore of an inconsistent snapshot = %v, want an error mentioning %q", err, testCase.want)
			}
			// the mismatch must be caught before any deletion: the live disk of
			// a node the caller never saw as vanishing would otherwise be gone
			for _, node := range item.Nodes {
				if got := readDisk(t, dir, node.Name); got != "live-"+node.Name {
					t.Errorf("node %s = %q, want the untouched live disk", node.Name, got)
				}
			}
			reloaded, err := Load("demo")
			if err != nil {
				t.Fatalf("live cluster state destroyed by a rejected restore: %v", err)
			}
			if len(reloaded.Nodes) != len(item.Nodes) {
				t.Fatalf("live node count after a rejected restore = %d, want %d", len(reloaded.Nodes), len(item.Nodes))
			}
			liveEntries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			liveImages := 0
			for _, entry := range liveEntries {
				if filepath.Ext(entry.Name()) == ".img" {
					liveImages++
				}
			}
			if liveImages != len(item.Nodes) {
				t.Fatalf("live disk image count after a rejected restore = %d, want %d", liveImages, len(item.Nodes))
			}
		})
	}
}

func TestListSnapshotsEmptyResultMarshalsAsArray(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	snapshots, err := ListSnapshots("no-such-cluster")
	if err != nil {
		t.Fatal(err)
	}
	if snapshots == nil {
		t.Fatal("ListSnapshots returned nil; daemon responses embedding it would marshal null instead of []")
	}
	encoded, err := json.Marshal(snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("empty snapshot list marshals as %s, want []", encoded)
	}
}
