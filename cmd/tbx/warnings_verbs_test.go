package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// Unrelated findings must reach the operator one per line, whichever verb
// raised them (#291): a semicolon-joined run-on hides the second one.
func TestNodeAddRendersEachWarningOnItsOwnLine(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"name":"demo-worker-3","warning":"first; second","warnings":["first","second"]}`)},
	})

	if err := command.runNode([]string{"add", "demo"}); err != nil {
		t.Fatal(err)
	}
	if got := command.err.(*bytes.Buffer).String(); got != "warning: first\nwarning: second\n" {
		t.Fatalf("stderr = %q, want one warning per line", got)
	}
}

func TestNodeRemoveRendersEachWarningOnItsOwnLine(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":7}`)},
		{OK: true, Data: json.RawMessage(`{"name":"demo-worker-2","warning":"first; second","warnings":["first","second"]}`)},
	})

	if err := command.runNode([]string{"remove", "demo", "demo-worker-2"}); err != nil {
		t.Fatal(err)
	}
	if got := command.err.(*bytes.Buffer).String(); got != "warning: first\nwarning: second\n" {
		t.Fatalf("stderr = %q, want one warning per line", got)
	}
}

func TestSnapshotCreateRendersEachWarningOnItsOwnLine(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":7}`)},
		{OK: true, Data: json.RawMessage(`{"snapshots":[{"name":"before"}],"warning":"first; second","warnings":["first","second"]}`)},
	})

	if err := command.snapshotCreate([]string{"demo", "before", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if got := command.err.(*bytes.Buffer).String(); got != "warning: first\nwarning: second\n" {
		t.Fatalf("stderr = %q, want one warning per line", got)
	}
}

func TestSnapshotRestoreRendersEachWarningOnItsOwnLine(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":7}`)},
		{OK: true, Data: json.RawMessage(`{"snapshots":[{"name":"before"}],"warning":"first; second","warnings":["first","second"]}`)},
	})

	if err := command.snapshotRestore([]string{"demo", "before", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if got := command.err.(*bytes.Buffer).String(); got != "warning: first\nwarning: second\n" {
		t.Fatalf("stderr = %q, want one warning per line", got)
	}
}

// TestNodeAddSurfacesTheBroadRouteWarningLikeCreate: a VPN utun holding a
// broader route over the cluster subnet is reported by cluster create, and the
// same finding travels in the node status, so node add must print it too — the
// operator is otherwise told about the capture only on the verb that happened
// to be first (#275).
func TestNodeAddSurfacesTheBroadRouteWarningLikeCreate(t *testing.T) {
	const broadRoute = "host route for 172.30.5.0/24 goes through utun9, which captures the cluster subnet"

	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})
	_, add := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"name":"demo-worker-3","warnings":["` + broadRoute + `"]}`)},
	})
	if err := add.runNode([]string{"add", "demo"}); err != nil {
		t.Fatal(err)
	}

	_, create := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"name":"demo","warnings":["` + broadRoute + `"]}`)},
	})
	if err := create.runCluster([]string{"start", "demo"}); err != nil {
		t.Fatal(err)
	}

	want := "warning: " + broadRoute + "\n"
	for verb, command := range map[string]cli{"node add": add, "cluster start": create} {
		if got := command.err.(*bytes.Buffer).String(); got != want {
			t.Errorf("tbx %s stderr = %q, want %q", verb, got, want)
		}
	}
}

// A daemon that predates the per-finding list still speaks only Warning, and
// its single joined string must still be printed.
func TestNodeAndSnapshotVerbsFallBackToTheLegacyJoinedWarning(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"name":"demo-worker-3","warning":"first; second"}`)},
	})

	if err := command.runNode([]string{"add", "demo"}); err != nil {
		t.Fatal(err)
	}
	if got := command.err.(*bytes.Buffer).String(); got != "warning: first; second\n" {
		t.Fatalf("stderr = %q, want the legacy joined warning", got)
	}
}
