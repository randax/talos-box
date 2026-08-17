package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// confirmIfRunning must decline on a non-"y" answer. Uses a stopped/absent
// cluster path indirectly is hard without a daemon, so we test the pure
// decision: --yes always proceeds; the reader gates otherwise. Here we exercise
// the reader branch by constructing a cli whose status call is bypassed via
// --yes true (proceed) and false-with-"n" is covered by the parse-level guard.
func TestConfirmYesSkips(t *testing.T) {
	c := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: strings.NewReader("")}
	if err := c.confirmIfRunning("demo", true, "snapshot"); err != nil {
		t.Errorf("--yes should skip confirmation, got %v", err)
	}
}

func TestSnapshotCreateConfirmationSaysTheClusterIsStopped(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":7}`)},
		{OK: true, Data: json.RawMessage(`[{"name":"demo","running":true}]`)},
		{OK: true, Data: json.RawMessage(`{"snapshots":[{"name":"baseline"}]}`)},
	})
	command.in = strings.NewReader("y\n")

	if err := command.snapshotCreate([]string{"demo", "baseline"}); err != nil {
		t.Fatal(err)
	}

	if op := (<-requests).Op; op != "daemon.info" {
		t.Fatalf("first request op = %q, want daemon.info handshake", op)
	}
	if op := (<-requests).Op; op != "status" {
		t.Fatalf("second request op = %q, want the running-cluster status check", op)
	}
	if op := (<-requests).Op; op != "snapshot.create" {
		t.Fatalf("third request op = %q, want snapshot.create", op)
	}
	prompt := command.err.(*bytes.Buffer).String()
	for _, want := range []string{"demo is running", "stop and restart it"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("confirmation %q does not mention %q", prompt, want)
		}
	}
}

func TestSnapshotCreateAbortsWhenTheStopIsDeclined(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":7}`)},
		{OK: true, Data: json.RawMessage(`[{"name":"demo","running":true}]`)},
	})
	command.in = strings.NewReader("n\n")

	err := command.snapshotCreate([]string{"demo", "baseline"})

	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("snapshot create with a declined stop = %v, want an abort", err)
	}
	if op := (<-requests).Op; op != "daemon.info" {
		t.Fatalf("first request op = %q, want daemon.info handshake", op)
	}
	if op := (<-requests).Op; op != "status" {
		t.Fatalf("second request op = %q, want only the status check", op)
	}
	select {
	case request := <-requests:
		t.Fatalf("declined confirmation still sent %q", request.Op)
	default:
	}
}

func TestDefaultSnapshotNameShape(t *testing.T) {
	name := defaultSnapshotName()
	if !strings.HasPrefix(name, "snap-") || len(name) != len("snap-20060102-150405") {
		t.Errorf("default snapshot name %q has unexpected shape", name)
	}
}

func TestSuspendResumeAreClusterVerbs(t *testing.T) {
	// unknown cluster verbs should error clearly; suspend/resume must be known
	for _, verb := range []string{"suspend", "resume"} {
		c := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: strings.NewReader("")}
		err := c.runCluster([]string{verb})
		if err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%s should be a known verb needing an arg, got %v", verb, err)
		}
	}
}
