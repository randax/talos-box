package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestDestroyClusterPrintsInspectWarningBeforeDestroy(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"warning":"destroying cluster demo will permanently delete 2 longhorn volumes and their data"}`)},
		{OK: true, Data: json.RawMessage(`{"name":"demo"}`)},
	})

	if err := command.destroyCluster([]string{"demo", "--force"}); err != nil {
		t.Fatal(err)
	}

	assertDestroyRequests(t, requests, "cluster.destroy.inspect", "cluster.destroy")
	if got := command.err.(*bytes.Buffer).String(); !strings.Contains(got, "2 longhorn volumes") {
		t.Fatalf("stderr = %q, want inspect warning", got)
	}
	if got := command.out.(*bytes.Buffer).String(); !strings.Contains(got, "destroyed cluster demo") {
		t.Fatalf("stdout = %q, want destroy confirmation", got)
	}
}

func TestDestroyClusterStillDestroysAfterInspectError(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: false, Error: "inspect failed"},
		{OK: true, Data: json.RawMessage(`{"name":"demo"}`)},
	})

	if err := command.destroyCluster([]string{"demo", "--force"}); err != nil {
		t.Fatal(err)
	}

	assertDestroyRequests(t, requests, "cluster.destroy.inspect", "cluster.destroy")
	if got := command.err.(*bytes.Buffer).String(); !strings.Contains(got, "may permanently delete CSI-backed data") {
		t.Fatalf("stderr = %q, want generic data-loss warning", got)
	}
}

func newDestroyTestCLI(t *testing.T, responses []daemon.Response) (chan daemon.Request, cli) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "tbx-destroy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket := filepath.Join(home, ".talosbox", "tbxd.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	requests := make(chan daemon.Request, len(responses))
	go func() {
		for _, response := range responses {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			var request daemon.Request
			if err := json.NewDecoder(connection).Decode(&request); err == nil {
				requests <- request
				_ = json.NewEncoder(connection).Encode(response)
			}
			_ = connection.Close()
		}
	}()

	var stdout, stderr bytes.Buffer
	return requests, cli{out: &stdout, err: &stderr}
}

func assertDestroyRequests(t *testing.T, requests <-chan daemon.Request, want ...string) {
	t.Helper()
	for index, op := range want {
		request := <-requests
		if request.Op != op {
			t.Fatalf("request %d op = %q, want %q", index, request.Op, op)
		}
		if op == "cluster.destroy.inspect" || op == "cluster.destroy" {
			var args struct {
				Name  string `json:"name"`
				Force bool   `json:"force"`
			}
			if err := json.Unmarshal(request.Args, &args); err != nil {
				t.Fatalf("decode %s args: %v", op, err)
			}
			if args.Name != "demo" || !args.Force {
				t.Fatalf("%s args = %+v, want demo force destroy", op, args)
			}
		}
	}
	if extra := len(requests); extra != 0 {
		t.Fatalf("unexpected extra requests: %d", extra)
	}
}
