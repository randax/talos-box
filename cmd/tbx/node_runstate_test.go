package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestNodeStartSendsClusterAndNodeAndConfirms(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":8}`)},
		{OK: true, Data: json.RawMessage(`{"name":"demo-worker-2","phase":"running"}`)},
	})

	if err := command.runNode([]string{"start", "demo", "demo-worker-2"}); err != nil {
		t.Fatal(err)
	}

	if op := (<-requests).Op; op != "daemon.info" {
		t.Fatalf("first request op = %q, want daemon.info handshake", op)
	}
	request := <-requests
	if request.Op != "node.start" {
		t.Fatalf("request op = %q, want node.start", request.Op)
	}
	var args struct {
		Cluster string `json:"cluster"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Cluster != "demo" || args.Name != "demo-worker-2" {
		t.Fatalf("node.start args = %+v, want demo/demo-worker-2", args)
	}
	if got := command.out.(*bytes.Buffer).String(); !strings.Contains(got, "started node demo-worker-2 in cluster demo") {
		t.Fatalf("stdout = %q, want start confirmation", got)
	}
}

func TestNodeStopConfirmsAndPrintsWarnings(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":8}`)},
		{OK: true, Data: json.RawMessage(`{"name":"demo-worker-2","warnings":["subnet 172.30.0.0/24 is also on docker0"]}`)},
	})

	if err := command.runNode([]string{"stop", "demo", "demo-worker-2"}); err != nil {
		t.Fatal(err)
	}

	<-requests
	if op := (<-requests).Op; op != "node.stop" {
		t.Fatalf("request op = %q, want node.stop", op)
	}
	if got := command.out.(*bytes.Buffer).String(); !strings.Contains(got, "stopped node demo-worker-2 in cluster demo") {
		t.Fatalf("stdout = %q, want stop confirmation", got)
	}
	if got := command.err.(*bytes.Buffer).String(); !strings.Contains(got, "docker0") {
		t.Fatalf("stderr = %q, want the daemon warning", got)
	}
}

func TestNodeRunStateRefusesOldDaemonProtocol(t *testing.T) {
	for _, verb := range []string{"start", "stop"} {
		_, command := newDestroyTestCLI(t, []daemon.Response{
			{OK: true, Data: json.RawMessage(`{"protocolVersion":7}`)},
		})

		err := command.runNode([]string{verb, "demo", "demo-worker-2"})
		if err == nil {
			t.Fatalf("node %s against a protocol-7 daemon succeeded, want handshake refusal", verb)
		}
		if !strings.Contains(err.Error(), "run: tbx system restart") {
			t.Fatalf("handshake error = %q, want upgrade guidance", err)
		}
	}
}

func TestNodeRunStateUsageRequiresClusterAndNode(t *testing.T) {
	for _, verb := range []string{"start", "stop"} {
		_, command := newDestroyTestCLI(t, nil)

		err := command.runNode([]string{verb, "demo"})
		if err == nil {
			t.Fatalf("node %s with one positional succeeded, want usage error", verb)
		}
		if !strings.Contains(err.Error(), "usage: tbx node "+verb+" <cluster> <node>") {
			t.Fatalf("usage error = %q, want the two-positional usage", err)
		}
	}
}

// TestNodeStartForwardsForce keeps the CLI able to reach the daemon's override:
// node start is gated on host memory and pressure like cluster start, so
// without a --force flag a refusal would have no way out.
func TestNodeStartForwardsForce(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"protocolVersion":8}`)},
		{OK: true, Data: json.RawMessage(`{"name":"demo-worker-2","phase":"running"}`)},
	})

	if err := command.runNode([]string{"start", "demo", "demo-worker-2", "--force"}); err != nil {
		t.Fatal(err)
	}

	<-requests
	request := <-requests
	var args struct {
		Force bool `json:"force"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if !args.Force {
		t.Fatalf("node.start args = %s, want force set", request.Args)
	}
}
