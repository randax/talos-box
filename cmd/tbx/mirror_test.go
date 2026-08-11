package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/daemon"
)

func TestRunMirrorOfflineReportsCurrentState(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	done := make(chan struct{})
	go serveSingleDaemonRequest(t, listener, func(request daemon.Request) daemon.Response {
		if request.Op != "mirror.offline.get" {
			t.Fatalf("request op = %q, want mirror.offline.get", request.Op)
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.MirrorOfflineStatus{Enabled: false})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.run([]string{"mirror", "offline"}); err != nil {
		t.Fatal(err)
	}
	<-done

	if got := stdout.String(); got != "mirror offline is off\n" {
		t.Fatalf("stdout = %q, want mirror offline is off", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunMirrorOfflineOnSetsAndReportsState(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	done := make(chan struct{})
	go serveSingleDaemonRequest(t, listener, func(request daemon.Request) daemon.Response {
		if request.Op != "mirror.offline.set" {
			t.Fatalf("request op = %q, want mirror.offline.set", request.Op)
		}
		var args struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if !args.Enabled {
			t.Fatal("mirror.offline.set enabled = false, want true")
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.MirrorOfflineStatus{Enabled: true})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.run([]string{"mirror", "offline", "on"}); err != nil {
		t.Fatal(err)
	}
	<-done

	if got := stdout.String(); got != "mirror offline is on\n" {
		t.Fatalf("stdout = %q, want mirror offline is on", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func serveSingleDaemonRequest(t *testing.T, listener net.Listener, respond func(daemon.Request) daemon.Response, done chan<- struct{}) {
	t.Helper()
	connection, err := listener.Accept()
	if err != nil {
		t.Error(err)
		close(done)
		return
	}
	defer func() { _ = connection.Close() }()
	defer close(done)

	var request daemon.Request
	if err := json.NewDecoder(connection).Decode(&request); err != nil {
		t.Error(err)
		return
	}
	if err := json.NewEncoder(connection).Encode(respond(request)); err != nil {
		t.Error(err)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func shortTestHome(t *testing.T) string {
	t.Helper()
	path := filepath.Join("/private/tmp", fmt.Sprintf("tbx132-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}
