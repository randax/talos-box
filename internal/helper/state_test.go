package helper

import (
	"os"
	"path/filepath"
	"testing"
)

func syncedFixture() []SyncedCluster {
	return []SyncedCluster{{
		Name:        "alpha",
		SubnetIndex: 0,
		Nodes: []SyncedNode{
			{Name: "alpha-cp-1", MAC: "52:54:00:00:00:01", IP: "172.30.0.11"},
		},
	}, {
		Name:        "beta",
		SubnetIndex: 3,
		Nodes: []SyncedNode{
			{Name: "beta-cp-1", MAC: "52:54:00:00:03:01", IP: "172.30.3.11"},
		},
	}}
}

func TestHelperStateRoundTripsThroughDisk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	state := newHelperState(dir)
	if err := state.Replace(syncedFixture()); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "reservations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("reservations.json mode = %o, want 600", mode)
	}

	reloaded := newHelperState(dir)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	clusters := reloaded.Clusters()
	if len(clusters) != 2 {
		t.Fatalf("clusters = %#v, want 2", clusters)
	}
	if clusters[0].Name != "alpha" || clusters[0].Nodes[0].MAC != "52:54:00:00:00:01" || clusters[0].Nodes[0].IP != "172.30.0.11" {
		t.Fatalf("first cluster = %#v", clusters[0])
	}
	if got := reloaded.SubnetIndexes(); len(got) != 2 || got[0] != 0 || got[1] != 3 {
		t.Fatalf("subnet indexes = %v, want [0 3]", got)
	}
}

func TestHelperStateLoadTreatsMissingFileAsEmpty(t *testing.T) {
	t.Parallel()

	state := newHelperState(t.TempDir())
	if err := state.Load(); err != nil {
		t.Fatal(err)
	}
	if got := state.Clusters(); len(got) != 0 {
		t.Fatalf("clusters = %#v, want none", got)
	}
	if got := state.SubnetIndexes(); len(got) != 0 {
		t.Fatalf("subnet indexes = %v, want none", got)
	}
}

func TestHelperStateReplaceRejectsInvalidReservations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	state := newHelperState(dir)
	if err := state.Replace(syncedFixture()); err != nil {
		t.Fatal(err)
	}
	duplicate := []SyncedCluster{{
		Name:        "gamma",
		SubnetIndex: 0,
		Nodes: []SyncedNode{
			{Name: "a", MAC: "52:54:00:00:00:09", IP: "172.30.0.21"},
			{Name: "b", MAC: "52:54:00:00:00:09", IP: "172.30.0.22"},
		},
	}}
	if err := state.Replace(duplicate); err == nil {
		t.Fatal("Replace accepted a duplicate MAC")
	}
	if got := state.Clusters(); len(got) != 2 || got[0].Name != "alpha" {
		t.Fatalf("clusters = %#v, want the previous set retained", got)
	}
	reloaded := newHelperState(dir)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Clusters(); len(got) != 2 {
		t.Fatalf("persisted clusters = %#v, want the previous set retained", got)
	}
}

func TestHelperStateWithoutDirectoryKeepsStateInMemory(t *testing.T) {
	t.Parallel()

	state := newHelperState("")
	if err := state.Load(); err != nil {
		t.Fatal(err)
	}
	if err := state.Replace(syncedFixture()); err != nil {
		t.Fatal(err)
	}
	if got := state.Clusters(); len(got) != 2 {
		t.Fatalf("clusters = %#v, want 2", got)
	}
}

func TestHelperStateDirPrefersSystemdStateDirectory(t *testing.T) {
	t.Parallel()

	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	if got := helperStateDir(env(map[string]string{
		"STATE_DIRECTORY":      "/var/lib/tbx",
		"TBX_HELPER_STATE_DIR": "/tmp/override",
	})); got != "/var/lib/tbx" {
		t.Fatalf("state dir = %q, want /var/lib/tbx", got)
	}
	if got := helperStateDir(env(map[string]string{"TBX_HELPER_STATE_DIR": "/tmp/override"})); got != "/tmp/override" {
		t.Fatalf("state dir = %q, want /tmp/override", got)
	}
	if got := helperStateDir(env(nil)); got != "" {
		t.Fatalf("state dir = %q, want empty", got)
	}
}

func TestHelperStateDirTakesTheFirstSystemdDirectory(t *testing.T) {
	t.Parallel()

	got := helperStateDir(func(name string) string {
		if name == "STATE_DIRECTORY" {
			return "/var/lib/tbx:/var/lib/other"
		}
		return ""
	})
	if got != "/var/lib/tbx" {
		t.Fatalf("state dir = %q, want /var/lib/tbx", got)
	}
}
