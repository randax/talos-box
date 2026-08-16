package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/daemon"
)

func TestUserSchematicOverrideReachesDaemonEntryPoints(t *testing.T) {
	const override = "user-supplied-schematic"
	tests := []struct {
		name     string
		response json.RawMessage
		run      func(cli) error
	}{
		{
			name:     "cluster create",
			response: json.RawMessage(`{"name":"demo","controlPlanes":1,"workers":2,"nodeDefaults":{"memoryMiB":2048,"cpus":2,"diskGiB":20},"schematic":"user-supplied-schematic"}`),
			run: func(command cli) error {
				return command.createCluster([]string{"demo", "--schematic=" + override})
			},
		},
		{
			name:     "cache pull",
			response: json.RawMessage(`{"schematic":"user-supplied-schematic","version":"v1.12.1","architecture":"arm64","path":"/cache/disk.raw"}`),
			run: func(command cli) error {
				return command.runCache([]string{"pull", "--schematic=" + override, "--talos-version=v1.12.1"})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := runWithDaemonResponse(t, test.response, test.run)
			var args map[string]json.RawMessage
			if err := json.Unmarshal(request.Args, &args); err != nil {
				t.Fatal(err)
			}
			if got := string(args["schematic"]); got != `"`+override+`"` {
				t.Fatalf("schematic request field = %s, want %q", got, override)
			}
		})
	}
}

func runWithDaemonResponse(t *testing.T, data json.RawMessage, run func(cli) error) daemon.Request {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "tbx-")
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

	type requestResult struct {
		request daemon.Request
		err     error
	}
	results := make(chan requestResult, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			results <- requestResult{err: err}
			return
		}
		defer func() { _ = connection.Close() }()
		var request daemon.Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			results <- requestResult{err: err}
			return
		}
		results <- requestResult{request: request}
		_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: data})
	}()

	var stdout, stderr bytes.Buffer
	if err := run(cli{out: &stdout, err: &stderr}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("receive daemon request: %v", result.err)
		}
		return result.request
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for daemon request")
		return daemon.Request{}
	}
}

func TestCreateClusterEchoesFileLevelTalosBlock(t *testing.T) {
	// A file-level pin is semantically identical for a single cluster and the
	// echoed file replays against daemons predating per-cluster talos.
	response := json.RawMessage(`{"name":"demo","controlPlanes":1,"workers":2,"nodeDefaults":{"memoryMiB":2048,"cpus":2,"diskGiB":20},"talosVersion":"v1.14.0","schematic":"user-supplied-schematic"}`)
	var stdout string
	runWithDaemonResponse(t, response, func(command cli) error {
		var buffer bytes.Buffer
		command.out = &buffer
		err := command.createCluster([]string{"demo", "--schematic=user-supplied-schematic", "--talos-version=v1.14.0"})
		stdout = buffer.String()
		return err
	})
	if wanted := "talos:\n  version: v1.14.0\n  schematic: user-supplied-schematic\n"; !strings.Contains(stdout, wanted) {
		t.Fatalf("create echo missing file-level talos block %q:\n%s", wanted, stdout)
	}
	if strings.Contains(stdout, "    talos:") {
		t.Fatalf("create echo emits a per-cluster talos stanza:\n%s", stdout)
	}
}

func TestCreateClusterTalosVersionFlagReachesTheWire(t *testing.T) {
	response := json.RawMessage(`{"name":"demo","controlPlanes":1,"workers":2,"nodeDefaults":{"memoryMiB":2048,"cpus":2,"diskGiB":20}}`)
	request := runWithDaemonResponse(t, response, func(command cli) error {
		return command.createCluster([]string{"demo", "--schematic=user-supplied-schematic", "--talos-version=v1.14.0"})
	})
	var args map[string]json.RawMessage
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if got := string(args["version"]); got != `"v1.14.0"` {
		t.Fatalf("version request field = %s, want v1.14.0", got)
	}
}
