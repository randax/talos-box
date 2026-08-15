package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestNodeRemoveSendsForceAndPrintsWarning(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"name":"demo-worker-2","warning":"removing node demo-worker-2 permanently deletes the only copy of 1 longhorn volume"}`)},
	})

	if err := command.runNode([]string{"remove", "demo", "demo-worker-2", "--force"}); err != nil {
		t.Fatal(err)
	}

	request := <-requests
	if request.Op != "node.remove" {
		t.Fatalf("request op = %q, want node.remove", request.Op)
	}
	var args struct {
		Cluster string `json:"cluster"`
		Name    string `json:"name"`
		Force   bool   `json:"force"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Cluster != "demo" || args.Name != "demo-worker-2" || !args.Force {
		t.Fatalf("node.remove args = %+v, want demo/demo-worker-2 with force", args)
	}
	if got := command.out.(*bytes.Buffer).String(); !strings.Contains(got, "removed node demo-worker-2 from cluster demo") {
		t.Fatalf("stdout = %q, want removal confirmation", got)
	}
	if got := command.err.(*bytes.Buffer).String(); !strings.Contains(got, "permanently deletes the only copy of 1 longhorn volume") {
		t.Fatalf("stderr = %q, want daemon warning", got)
	}
}

func TestNodeRemoveDefaultsToUnforcedAndStaysQuietWithoutWarning(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"name":"demo-worker-2"}`)},
	})

	if err := command.runNode([]string{"remove", "demo", "demo-worker-2"}); err != nil {
		t.Fatal(err)
	}

	request := <-requests
	var args struct {
		Force bool `json:"force"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Force {
		t.Fatal("node.remove sent force=true without --force")
	}
	if got := command.err.(*bytes.Buffer).String(); got != "" {
		t.Fatalf("stderr = %q, want no warning output", got)
	}
}

func TestNodeRemoveUsageMentionsForce(t *testing.T) {
	_, command := newDestroyTestCLI(t, nil)

	err := command.runNode([]string{"remove", "demo"})
	if err == nil {
		t.Fatal("node remove with one positional succeeded, want usage error")
	}
	if !strings.Contains(err.Error(), "usage: tbx node remove <cluster> <node> [--force]") {
		t.Fatalf("usage error = %q, want usage with --force", err)
	}
}
