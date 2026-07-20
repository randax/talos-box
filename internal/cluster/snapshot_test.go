package cluster

import (
	"os"
	"path/filepath"
	"sort"
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
