package daemon

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/hypervisor"
)

// TestSuspendedNodeReportsTheSuspendedPhase pins #415: the table has promoted a
// stopped node holding saved memory to "suspended" since #360, while the JSON
// surface still said "stopped" — so a consumer keying on phase alone misread a
// suspended cluster as stopped.
func TestSuspendedNodeReportsTheSuspendedPhase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := seedStoppedCluster(t, "napping")
	if err := os.WriteFile(saveStatePath(dir, "napping-cp-1"), []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}

	service := &Server{vms: make(map[string]map[string]hypervisor.Machine)}
	statuses, err := service.status(mustRawJSON(t, statusArgs{Cluster: "napping"}))
	if err != nil {
		t.Fatal(err)
	}
	node := statuses[0].Nodes[0]
	if node.Phase != PhaseSuspended {
		t.Fatalf("node phase = %q, want %q", node.Phase, PhaseSuspended)
	}
	// The boolean stays: it is the same fact, and older clients read only it.
	if !node.Suspended {
		t.Fatal("node.Suspended = false beside the suspended phase")
	}
	if !PhaseSuspended.Stopped() {
		t.Fatal("PhaseSuspended must still count as a node with no running VM")
	}
}

// TestStoppedNodeWithoutSavedMemoryStaysStopped keeps the promotion honest: a
// member suspend never saved is plain stopped, and a resume cold-boots it.
func TestStoppedNodeWithoutSavedMemoryStaysStopped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedStoppedCluster(t, "idle")

	service := &Server{vms: make(map[string]map[string]hypervisor.Machine)}
	statuses, err := service.status(mustRawJSON(t, statusArgs{Cluster: "idle"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses[0].Nodes[0].Phase; got != PhaseStopped {
		t.Fatalf("node phase = %q, want %q", got, PhaseStopped)
	}
}

// TestSuspendHintNamesColdBootOnceTheOwningDaemonIsGone pins #413: after
// `tbx system restart --force` replaced the daemon that wrote the save, the
// memory is already unrestorable — the hint must say so instead of contrasting
// resume with start as if there were something left to lose.
func TestSuspendHintNamesColdBootOnceTheOwningDaemonIsGone(t *testing.T) {
	for _, test := range []struct {
		name       string
		owner      string
		wantStale  bool
		wantPhrase string
	}{
		{name: "owner replaced", owner: "999999", wantStale: true, wantPhrase: "will cold-boot the nodes"},
		{name: "this daemon still owns it", owner: daemonInstanceToken(), wantPhrase: "tbx cluster start discards the saved memory"},
		{name: "owner is a predecessor holding this pid", owner: strconv.Itoa(os.Getpid()), wantStale: true, wantPhrase: "will cold-boot the nodes"},
		{name: "owner unknown", wantPhrase: "tbx cluster start discards the saved memory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dir := seedStoppedCluster(t, "qa-sta")
			save := saveStatePath(dir, "qa-sta-cp-1")
			if err := os.WriteFile(save, []byte("saved"), 0o600); err != nil {
				t.Fatal(err)
			}
			if test.owner != "" {
				if err := os.WriteFile(saveStateOwnerPath(save), []byte(test.owner), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			service := &Server{vms: make(map[string]map[string]hypervisor.Machine)}
			statuses, err := service.status(mustRawJSON(t, statusArgs{Cluster: "qa-sta"}))
			if err != nil {
				t.Fatal(err)
			}
			status := statuses[0]
			if status.SavedStateStale != test.wantStale {
				t.Fatalf("SavedStateStale = %t, want %t", status.SavedStateStale, test.wantStale)
			}
			joined := strings.Join(status.Hints, "\n")
			if !strings.Contains(joined, test.wantPhrase) {
				t.Fatalf("hints = %q, want %q", joined, test.wantPhrase)
			}
			if test.wantStale && strings.Contains(joined, "tbx cluster start discards the saved memory") {
				t.Fatalf("stale save still promised restorable memory: %s", joined)
			}
		})
	}
}

// TestSavedStateOwnerIsIgnoredWithoutItsSave keeps a leftover sidecar from
// answering for memory nobody holds.
func TestSavedStateOwnerIsIgnoredWithoutItsSave(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := seedStoppedCluster(t, "orphan")
	if err := os.WriteFile(saveStateOwnerPath(saveStatePath(dir, "orphan-cp-1")), []byte("999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	if savedStateOwnerReplaced("orphan") {
		t.Fatal("an owner sidecar with no save reported stale memory")
	}
}
