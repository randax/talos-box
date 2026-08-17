package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/daemon"
)

// stubSupervisorCommand replaces the service-manager probe with a scripted one
// and records every command it was asked to run.
func stubSupervisorCommand(t *testing.T, answer func(name string, args []string) error) *[][]string {
	t.Helper()
	var invoked [][]string
	previous := runSupervisorCommand
	t.Cleanup(func() { runSupervisorCommand = previous })
	runSupervisorCommand = func(name string, args ...string) error {
		invoked = append(invoked, append([]string{name}, args...))
		return answer(name, args)
	}
	return &invoked
}

func TestSupervisedDaemonAsksTheServiceManager(t *testing.T) {
	tempHome(t)
	invoked := stubSupervisorCommand(t, func(string, []string) error { return nil })

	supervised, reason := supervisedDaemon()
	if !supervised {
		t.Fatal("a supervisor that reports tbxd active must count as supervised")
	}
	if len(*invoked) == 0 {
		t.Fatal("supervision detection must ask the supervisor, not only stat unit files")
	}
	first := (*invoked)[0]
	switch runtime.GOOS {
	case "linux":
		want := []string{"systemctl", "--user", "is-active", "--quiet", "tbxd.socket"}
		if !slices.Equal(first, want) {
			t.Fatalf("command = %v, want %v", first, want)
		}
		if !strings.Contains(reason, "systemd") {
			t.Fatalf("reason = %q, want systemd named", reason)
		}
	case "darwin":
		if first[0] != "launchctl" || first[1] != "print" || !strings.Contains(first[2], "dev.talosbox.tbxd") {
			t.Fatalf("command = %v, want a launchctl print of the tbxd label", first)
		}
		if !strings.Contains(reason, "launchd") {
			t.Fatalf("reason = %q, want launchd named", reason)
		}
	}
}

func TestSupervisedDaemonFallsThroughWhenTheSupervisorIsUnusable(t *testing.T) {
	tempHome(t)
	stubSupervisorCommand(t, func(string, []string) error { return exec.ErrNotFound })

	if supervised, reason := supervisedDaemon(); supervised {
		t.Fatalf("supervisedDaemon() = true, %q, want false when nothing supervises tbxd", reason)
	}
}

func TestSupervisedDaemonDetectsAPackagedUserUnit(t *testing.T) {
	home := tempHome(t)
	stubSupervisorCommand(t, func(string, []string) error { return exec.ErrNotFound })
	unit := filepath.Join(home, ".config", "systemd", "user", "tbxd.socket")
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Socket]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	supervised, reason := supervisedDaemon()
	if !supervised || !strings.Contains(reason, unit) {
		t.Fatalf("supervisedDaemon() = %v, %q, want the socket unit path", supervised, reason)
	}
}

func TestSupervisionUnitPathsCoverThePackagedInstalls(t *testing.T) {
	got := supervisionUnitPaths()
	for _, want := range []string{
		"/usr/lib/systemd/user/tbxd.service",
		"/usr/lib/systemd/user/tbxd.socket",
		"/usr/local/lib/systemd/user/tbxd.service",
		"/etc/systemd/user/tbxd.service",
		"/etc/systemd/system/tbxd.service",
		"/Library/LaunchDaemons/dev.talosbox.tbxd.plist",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("supervisionUnitPaths() = %v, want %s included", got, want)
		}
	}
}

// TestWaitForDaemonExitWatchesThePIDNotTheSocket pins the socket-activation
// fix: dialing the socket to detect the exit would re-activate the very daemon
// being retired, so the wait must end once the process is gone even while the
// socket is still served.
func TestWaitForDaemonExitWatchesThePIDNotTheSocket(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	previous := daemonProcessAlive
	t.Cleanup(func() { daemonProcessAlive = previous })
	daemonProcessAlive = func(pid int) bool {
		if pid != 4242 {
			t.Errorf("polled pid = %d, want the daemon pid 4242", pid)
		}
		return false
	}

	if err := waitForDaemonExit(4242); err != nil {
		t.Fatalf("waitForDaemonExit = %v, want nil once the process is gone", err)
	}
	if slices.Contains(fake.recordedOps(), "daemon.info") {
		t.Fatal("waiting for the exit must not dial the socket")
	}
}

func TestCallRefusesToRestartAStaleDaemonWithRunningClusters(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	fake.runs("alpha", "beta")
	terminated := stubDaemonRestart(t, fake, unsupervisedDaemon)
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	err := command.call("status", struct{}{}, nil)
	if err == nil {
		t.Fatal("a read-only verb must not silently power off running clusters")
	}
	for _, want := range []string{"alpha", "beta", "tbx system restart --force", "stops them"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
	if *terminated != 0 {
		t.Fatal("the stale daemon must be left running")
	}
}

func TestCallRefusesToRestartWhenTheRunningStateIsUnknown(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	terminated := stubDaemonRestart(t, fake, unsupervisedDaemon)
	stubRunningClusters(t, func(string) ([]string, error) { return nil, errors.New("cluster.list refused") })
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	err := command.call("status", struct{}{}, nil)
	if err == nil || !strings.Contains(err.Error(), "could not tell whether clusters are running") {
		t.Fatalf("error = %v, want a refusal on unknown running state", err)
	}
	if *terminated != 0 {
		t.Fatal("an unknown running state must not be terminated through")
	}
}

func stubRunningClusters(t *testing.T, query func(string) ([]string, error)) {
	t.Helper()
	previous := runningClustersQuery
	t.Cleanup(func() { runningClustersQuery = previous })
	runningClustersQuery = query
}

func TestSystemRestartRefusesRunningClustersWithoutForce(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	fake.runs("alpha")
	terminated := stubDaemonRestart(t, fake, unsupervisedDaemon)
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.run([]string{"system", "restart"})
	if err == nil || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error = %v, want a refusal naming the running cluster and --force", err)
	}
	if *terminated != 0 {
		t.Fatal("an unforced restart must not stop a running cluster")
	}
}

func TestSystemRestartWithForceStopsAndReportsRunningClusters(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	fake.runs("alpha", "beta")
	terminated := stubDaemonRestart(t, fake, unsupervisedDaemon)
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	if err := command.run([]string{"system", "restart", "--force"}); err != nil {
		t.Fatal(err)
	}
	if *terminated != 1 {
		t.Fatalf("terminate calls = %d, want 1", *terminated)
	}
	got := stdout.String()
	if !strings.Contains(got, "stopped running clusters: alpha, beta") {
		t.Fatalf("stdout = %q, want the stopped clusters reported", got)
	}
	if !strings.Contains(got, "restarted tbxd") {
		t.Fatalf("stdout = %q, want the restart confirmation", got)
	}
}

func TestSystemRestartRejectsAnUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.run([]string{"system", "restart", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "usage: tbx system restart [--force]") {
		t.Fatalf("error = %v, want the restart usage", err)
	}
}

// scriptedDaemon serves the socket with a per-connection script, which is how
// draining and busy daemons are reproduced.
type scriptedDaemon struct {
	socket   string
	mu       sync.Mutex
	handled  int
	ops      []string
	listener net.Listener
}

func newScriptedDaemon(t *testing.T, handle func(index int, connection net.Conn, request *daemon.Request)) *scriptedDaemon {
	t.Helper()
	home := tempHome(t)
	socket := filepath.Join(home, ".talosbox", "tbxd.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	scripted := &scriptedDaemon{socket: socket, listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			var request daemon.Request
			decoded := json.NewDecoder(connection).Decode(&request) == nil
			scripted.mu.Lock()
			index := scripted.handled
			scripted.handled++
			if decoded {
				scripted.ops = append(scripted.ops, request.Op)
			}
			scripted.mu.Unlock()
			var decodedRequest *daemon.Request
			if decoded {
				decodedRequest = &request
			}
			// each connection is handled concurrently, so a blocked
			// daemon.info cannot stall the next verb's connection
			go handle(index, connection, decodedRequest)
		}
	}()
	return scripted
}

func (s *scriptedDaemon) recordedOps() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

// TestHandshakeDoesNotMemoizeATransientFailure pins that a draining daemon's
// EOF is not frozen for the whole process: the next verb handshakes again.
func TestHandshakeDoesNotMemoizeATransientFailure(t *testing.T) {
	scripted := newScriptedDaemon(t, func(index int, connection net.Conn, request *daemon.Request) {
		defer func() { _ = connection.Close() }()
		if index == 0 {
			// a draining daemon hangs up before answering daemon.info
			return
		}
		if request != nil && request.Op == "daemon.info" {
			data, _ := json.Marshal(daemon.Info{ProtocolVersion: daemon.ProtocolVersion})
			_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: data})
			return
		}
		_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: json.RawMessage(`{}`)})
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	if err := command.call("status", struct{}{}, nil); err != nil {
		t.Fatalf("first call = %v, want the transient handshake failure to be skipped", err)
	}
	if err := command.call("status", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}

	ops := scripted.recordedOps()
	handshakes := 0
	for _, op := range ops {
		if op == "daemon.info" {
			handshakes++
		}
	}
	if handshakes < 2 {
		t.Fatalf("ops = %v, want the handshake retried after a transient failure", ops)
	}
}

// TestHandshakeSkipsTheGateWhenTheDaemonIsBusy pins that daemon.info blocked
// behind a long operation cannot hang a verb.
func TestHandshakeSkipsTheGateWhenTheDaemonIsBusy(t *testing.T) {
	shortenHandshakeTimeout(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	newScriptedDaemon(t, func(_ int, connection net.Conn, request *daemon.Request) {
		if request != nil && request.Op == "daemon.info" {
			// daemon.info waits on the daemon's operation lock
			<-release
			_ = connection.Close()
			return
		}
		_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: json.RawMessage(`{}`)})
		_ = connection.Close()
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	done := make(chan error, 1)
	go func() { done <- command.call("status", struct{}{}, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("call under a busy daemon = %v, want the verb to proceed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a busy daemon hung the protocol gate")
	}
}

func TestSystemStatusReportsABusyDaemon(t *testing.T) {
	shortenHandshakeTimeout(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	newScriptedDaemon(t, func(_ int, connection net.Conn, _ *daemon.Request) {
		<-release
		_ = connection.Close()
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	done := make(chan error, 1)
	go func() { done <- command.run([]string{"system", "status"}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tbx system status hung on a busy daemon")
	}
	if got := stdout.String(); !strings.Contains(got, "tbxd: busy") ||
		!strings.Contains(got, "pid") {
		t.Fatalf("stdout = %q, want a busy report naming the pid", got)
	}
}

func shortenHandshakeTimeout(t *testing.T) {
	t.Helper()
	previous := daemonHandshakeTimeout
	t.Cleanup(func() { daemonHandshakeTimeout = previous })
	daemonHandshakeTimeout = 200 * time.Millisecond
}
