package helper

import (
	"os"
	"path/filepath"
	"strings"
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
	state := NewState(dir)
	if err := state.Replace(0, syncedFixture()); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "reservations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("reservations.json mode = %o, want 600", mode)
	}

	reloaded := NewState(dir)
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

	state := NewState(t.TempDir())
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
	state := NewState(dir)
	if err := state.Replace(0, syncedFixture()); err != nil {
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
	if err := state.Replace(0, duplicate); err == nil {
		t.Fatal("Replace accepted a duplicate MAC")
	}
	if got := state.Clusters(); len(got) != 2 || got[0].Name != "alpha" {
		t.Fatalf("clusters = %#v, want the previous set retained", got)
	}
	reloaded := NewState(dir)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Clusters(); len(got) != 2 {
		t.Fatalf("persisted clusters = %#v, want the previous set retained", got)
	}
}

func TestHelperStateWithoutDirectoryKeepsStateInMemory(t *testing.T) {
	t.Parallel()

	state := NewState("")
	if err := state.Load(); err != nil {
		t.Fatal(err)
	}
	if err := state.Replace(0, syncedFixture()); err != nil {
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

func TestHelperStateLoadTreatsACorruptFileAsEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reservations.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := NewState(dir)
	if err := state.Load(); err != nil {
		t.Fatalf("Load() error = %v, want a corrupt file to be tolerated", err)
	}
	if got := state.SubnetIndexes(); len(got) != 0 {
		t.Fatalf("subnets after corrupt load = %v, want none", got)
	}
}

// Two tbx group members each run a daemon over their own home. A sync from
// one replaces only that user's partition; the helper serves the union.
func TestHelperStateKeepsEachOwnersPartitionApart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	state := NewState(dir)
	alice := []SyncedCluster{{Name: "alpha", SubnetIndex: 0, Nodes: []SyncedNode{{Name: "alpha-cp-1", MAC: "52:54:00:00:00:01", IP: "172.30.0.11"}}}}
	bob := []SyncedCluster{{Name: "beta", SubnetIndex: 5, Nodes: []SyncedNode{{Name: "beta-cp-1", MAC: "52:54:00:00:05:01", IP: "172.30.5.11"}}}}
	if err := state.Replace(1000, alice); err != nil {
		t.Fatal(err)
	}
	if err := state.Replace(1001, bob); err != nil {
		t.Fatal(err)
	}
	if got := state.SubnetIndexes(); len(got) != 2 || got[0] != 0 || got[1] != 5 {
		t.Fatalf("subnets after two owners synced = %v, want [0 5]", got)
	}

	// Alice's daemon restarts with no clusters: Bob's must survive.
	if err := state.Replace(1000, nil); err != nil {
		t.Fatal(err)
	}
	if got := state.SubnetIndexes(); len(got) != 1 || got[0] != 5 {
		t.Fatalf("subnets after alice emptied hers = %v, want [5]", got)
	}

	// And so must both partitions across a helper restart.
	if err := state.Replace(1000, alice); err != nil {
		t.Fatal(err)
	}
	reloaded := NewState(dir)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.SubnetIndexes(); len(got) != 2 || got[0] != 0 || got[1] != 5 {
		t.Fatalf("subnets after reload = %v, want [0 5]", got)
	}
}

// A second user cannot take over an IP or MAC the first already reserved.
func TestHelperStateRejectsACrossOwnerCollision(t *testing.T) {
	t.Parallel()

	state := NewState("")
	alice := []SyncedCluster{{Name: "alpha", SubnetIndex: 0, Nodes: []SyncedNode{{Name: "alpha-cp-1", MAC: "52:54:00:00:00:01", IP: "172.30.0.11"}}}}
	bob := []SyncedCluster{{Name: "beta", SubnetIndex: 0, Nodes: []SyncedNode{{Name: "beta-cp-1", MAC: "52:54:00:00:00:02", IP: "172.30.0.11"}}}}
	if err := state.Replace(1000, alice); err != nil {
		t.Fatal(err)
	}
	err := state.Replace(1001, bob)
	if err == nil || !strings.Contains(err.Error(), "another user") {
		t.Fatalf("Replace() error = %v, want a cross-owner collision", err)
	}
	if got := state.Clusters(); len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("clusters after rejected sync = %+v, want alice's alone", got)
	}
}

// One name, one owner: attachments and speakers are addressed by cluster
// name, so a second user cannot sync a cluster under a name the first holds.
func TestHelperStateRefusesTheSameClusterNameUnderTwoOwners(t *testing.T) {
	t.Parallel()

	state := NewState("")
	alice := []SyncedCluster{{Name: "demo", SubnetIndex: 0, Nodes: []SyncedNode{{Name: "demo-cp-1", MAC: "52:54:00:00:00:01", IP: "172.30.0.11"}}}}
	bob := []SyncedCluster{{Name: "demo", SubnetIndex: 4, Nodes: []SyncedNode{{Name: "demo-cp-1", MAC: "52:54:00:00:04:01", IP: "172.30.4.11"}}}}
	if err := state.Replace(1000, alice); err != nil {
		t.Fatal(err)
	}
	err := state.Replace(1001, bob)
	if err == nil || !strings.Contains(err.Error(), `cluster name "demo"`) {
		t.Fatalf("Replace() error = %v, want the name refused", err)
	}
	// The same owner re-pushing the same name is, of course, fine.
	if err := state.Replace(1000, alice); err != nil {
		t.Fatal(err)
	}
}
