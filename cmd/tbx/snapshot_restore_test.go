package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestSnapshotRestoreSendsForceAndPrintsWarning(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":6}`)},
		{OK: true, Data: json.RawMessage(`{"snapshots":[{"name":"before"}],"warning":"restoring snapshot before permanently deletes longhorn volume data on demo-worker-2 (1 volume)"}`)},
	})

	if err := command.snapshotRestore([]string{"demo", "before", "--yes", "--force"}); err != nil {
		t.Fatal(err)
	}

	if op := (<-requests).Op; op != "daemon.info" {
		t.Fatalf("first request op = %q, want daemon.info handshake", op)
	}
	request := <-requests
	if request.Op != "snapshot.restore" {
		t.Fatalf("request op = %q, want snapshot.restore", request.Op)
	}
	var args struct {
		Cluster string `json:"cluster"`
		Name    string `json:"name"`
		Force   bool   `json:"force"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Cluster != "demo" || args.Name != "before" || !args.Force {
		t.Fatalf("snapshot.restore args = %+v, want demo/before with force", args)
	}
	if got := command.out.(*bytes.Buffer).String(); !strings.Contains(got, "restored demo from snapshot before") {
		t.Fatalf("stdout = %q, want restore confirmation", got)
	}
	if got := command.err.(*bytes.Buffer).String(); !strings.Contains(got, "permanently deletes longhorn volume data on demo-worker-2 (1 volume)") {
		t.Fatalf("stderr = %q, want daemon warning", got)
	}
}

func TestSnapshotRestoreDefaultsToUnforcedAndStaysQuietWithoutWarning(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":6}`)},
		{OK: true, Data: json.RawMessage(`{"snapshots":[{"name":"before"}]}`)},
	})

	if err := command.snapshotRestore([]string{"demo", "before", "--yes"}); err != nil {
		t.Fatal(err)
	}

	if op := (<-requests).Op; op != "daemon.info" {
		t.Fatalf("first request op = %q, want daemon.info handshake", op)
	}
	request := <-requests
	var args struct {
		Force bool `json:"force"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Force {
		t.Fatal("snapshot.restore sent force=true without --force")
	}
	if got := command.err.(*bytes.Buffer).String(); got != "" {
		t.Fatalf("stderr = %q, want no warning output", got)
	}
}

func TestSnapshotRestoreUsageMentionsForce(t *testing.T) {
	_, command := newDestroyTestCLI(t, nil)

	err := command.snapshotRestore([]string{"demo"})
	if err == nil {
		t.Fatal("snapshot restore with one positional succeeded, want usage error")
	}
	if !strings.Contains(err.Error(), "usage: tbx snapshot restore <cluster> <name> [--yes] [--force]") {
		t.Fatalf("usage error = %q, want usage with --force", err)
	}
}

func TestSnapshotRestoreRefusesOldDaemonProtocol(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":5}`)},
	})

	err := command.snapshotRestore([]string{"demo", "before", "--yes"})
	if err == nil {
		t.Fatal("snapshot restore against a protocol-5 daemon succeeded, want handshake refusal")
	}
	if !strings.Contains(err.Error(), "restart or upgrade tbxd") {
		t.Fatalf("handshake error = %q, want upgrade guidance", err)
	}
}
