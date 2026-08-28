package main

import (
	"bytes"
	"context"
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
	"sync/atomic"
	"syscall"
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

	state, reason := supervisedDaemon()
	if state != supervisionConfirmed {
		t.Fatal("a supervisor that reports tbxd active must count as confirmed supervision")
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

	if state, reason := supervisedDaemon(); state != supervisionNone {
		t.Fatalf("supervisedDaemon() = %v, %q, want none when nothing supervises tbxd", state, reason)
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

	state, reason := supervisedDaemon()
	if state != supervisionInferred || !strings.Contains(reason, unit) {
		t.Fatalf("supervisedDaemon() = %v, %q, want inferred supervision naming the socket unit path", state, reason)
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

	if err := waitForDaemonExit(4242, clusterActivity{}, nil); err != nil {
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
	for _, want := range []string{"alpha", "beta", "tbx system restart --force", "restarting tbxd stops these clusters"} {
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
	stubRunningClusters(t, func(string) (clusterActivity, error) {
		return clusterActivity{}, errors.New("cluster.list refused")
	})
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

func stubRunningClusters(t *testing.T, query func(string) (clusterActivity, error)) {
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
	if !strings.Contains(got, "stopped clusters: alpha, beta") {
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
	if got := stdout.String(); !strings.Contains(got, "runtime:") ||
		!strings.Contains(got, "daemon: busy") ||
		!strings.Contains(got, "pid") {
		t.Fatalf("stdout = %q, want a bounded runtime report naming the busy daemon pid", got)
	}
}

func TestSystemStatusUsesRuntimeIdentityBlock(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{
		out: &stdout,
		err: &stderr,
		runtimeIdentity: func(context.Context) runtimeIdentity {
			return runtimeIdentity{
				Client: componentIdentity{Name: "client", Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
				Daemon: componentIdentity{Name: "daemon", Path: "/opt/current/tbxd", Version: "0.1.3", Protocol: daemon.ProtocolVersion, PID: 1234, Available: true},
				Helper: componentIdentity{Name: "helper", Available: false, Detail: "not installed"},
				Findings: []doctorFinding{
					{level: "WARN", check: "installations", detail: "daemon executable /opt/current/tbxd does not match this client's sibling /opt/other/tbxd"},
				},
			}
		},
	}

	if err := command.run([]string{"system", "status"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "runtime:\n") ||
		!strings.Contains(got, "client: /opt/current/tbx") ||
		!strings.Contains(got, "daemon: /opt/current/tbxd") ||
		!strings.Contains(got, "helper: not installed") {
		t.Fatalf("stdout = %q, want the shared runtime identity block", got)
	}
	if got := stderr.String(); !strings.Contains(got, "WARN installations: daemon executable /opt/current/tbxd does not match this client's sibling /opt/other/tbxd") {
		t.Fatalf("stderr = %q, want runtime findings on stderr", got)
	}
}

func shortenHandshakeTimeout(t *testing.T) {
	t.Helper()
	previous, previousRetry := daemonHandshakeTimeout, daemonHandshakeRetryTimeout
	t.Cleanup(func() { daemonHandshakeTimeout, daemonHandshakeRetryTimeout = previous, previousRetry })
	daemonHandshakeTimeout = 200 * time.Millisecond
	daemonHandshakeRetryTimeout = 400 * time.Millisecond
}

// stubRestartProcesses replaces the process-level restart seams for a daemon
// that is not a fakeDaemon: the scripted daemon keeps its own listener, so the
// "replacement" is only a flag its handler reads.
func stubRestartProcesses(t *testing.T, onSpawn func()) *int {
	t.Helper()
	terminated := 0
	previousTerminate, previousSpawn := terminateDaemonProcess, spawnDaemonProcess
	previousSupervised, previousAlive := supervisedDaemon, daemonProcessAlive
	t.Cleanup(func() {
		terminateDaemonProcess, spawnDaemonProcess = previousTerminate, previousSpawn
		supervisedDaemon, daemonProcessAlive = previousSupervised, previousAlive
	})
	terminateDaemonProcess = func(int) error {
		terminated++
		return nil
	}
	daemonProcessAlive = func(int) bool { return terminated == 0 }
	spawnDaemonProcess = func() (int64, error) {
		onSpawn()
		return 0, nil
	}
	supervisedDaemon = unsupervisedDaemon
	return &terminated
}

// TestSystemRestartWithForceCompletesWhileTheDaemonIsBusy pins the recovery
// path's whole point: a daemon wedged under its operation lock answers neither
// daemon.info nor cluster.list, and --force must still finish in bounded time.
func TestSystemRestartWithForceCompletesWhileTheDaemonIsBusy(t *testing.T) {
	shortenHandshakeTimeout(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var replaced atomic.Bool
	newScriptedDaemon(t, func(_ int, connection net.Conn, request *daemon.Request) {
		defer func() { _ = connection.Close() }()
		if request == nil {
			return
		}
		if !replaced.Load() {
			// every op waits on the daemon's operation lock
			<-release
			return
		}
		if request.Op == "daemon.info" {
			data, _ := json.Marshal(daemon.Info{ProtocolVersion: daemon.ProtocolVersion})
			_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: data})
			return
		}
		_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: json.RawMessage(`{}`)})
	})
	terminated := stubRestartProcesses(t, func() { replaced.Store(true) })
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	done := make(chan error, 1)
	go func() { done <- command.run([]string{"system", "restart", "--force"}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("forced restart of a busy daemon = %v, want it to proceed", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("tbx system restart --force hung on a busy daemon")
	}
	if *terminated != 1 {
		t.Fatalf("terminate calls = %d, want 1", *terminated)
	}
	got := stdout.String()
	if !strings.Contains(got, "stopped clusters: unknown (state query failed)") {
		t.Fatalf("stdout = %q, want the unknown cluster state reported", got)
	}
	if !strings.Contains(got, "restarted tbxd") {
		t.Fatalf("stdout = %q, want the restart confirmation", got)
	}
}

// TestGateRefusesAStaleDaemonWhoseClusterListIsBlocked pins that the running
// -cluster query cannot hang a read-only verb: cluster.list is served under the
// same operation lock, so it needs its own deadline and an answer.
func TestGateRefusesAStaleDaemonWhoseClusterListIsBlocked(t *testing.T) {
	shortenHandshakeTimeout(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	newScriptedDaemon(t, func(_ int, connection net.Conn, request *daemon.Request) {
		defer func() { _ = connection.Close() }()
		if request == nil {
			return
		}
		if request.Op == "daemon.info" {
			data, _ := json.Marshal(daemon.Info{ProtocolVersion: daemon.ProtocolVersion - 1})
			_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: data})
			return
		}
		// cluster.list waits on the daemon's operation lock
		<-release
	})
	terminated := stubRestartProcesses(t, func() { t.Error("a daemon with unknown cluster state must not be replaced") })
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	done := make(chan error, 1)
	go func() { done <- command.call("status", struct{}{}, nil) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "could not tell whether clusters are running") {
			t.Fatalf("error = %v, want a refusal on unknown running state", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a blocked cluster.list hung the protocol gate")
	}
	if *terminated != 0 {
		t.Fatal("the stale daemon must be left running")
	}
}

// TestCallRefusesToRestartAStaleDaemonWithASuspendedCluster pins that a
// suspended cluster counts exactly like a running one: its saved memory does
// not survive the daemon that wrote it.
func TestCallRefusesToRestartAStaleDaemonWithASuspendedCluster(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	fake.suspends("gamma")
	terminated := stubDaemonRestart(t, fake, unsupervisedDaemon)
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	err := command.call("status", struct{}{}, nil)
	if err == nil {
		t.Fatal("a suspended cluster must not be restarted through")
	}
	for _, want := range []string{"gamma", "suspended", "lose their saved memory", "tbx system restart --force"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
	if *terminated != 0 {
		t.Fatal("the stale daemon must be left running")
	}
}

// TestCallRefusesToRestartWhenSavedStateIsOnDisk covers the blind spot a stale
// daemon creates: it predates ClusterSummary.Suspended, so it reports a
// suspended cluster as merely stopped and the CLI must find the saved memory
// itself.
func TestCallRefusesToRestartWhenSavedStateIsOnDisk(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	terminated := stubDaemonRestart(t, fake, unsupervisedDaemon)
	home := os.Getenv("HOME")
	saved := filepath.Join(home, ".talosbox", "clusters", "delta")
	if err := os.MkdirAll(saved, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saved, "cp1"+savedStateSuffix), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	err := command.call("status", struct{}{}, nil)
	if err == nil || !strings.Contains(err.Error(), "delta") {
		t.Fatalf("error = %v, want a refusal naming the cluster with saved state on disk", err)
	}
	if *terminated != 0 {
		t.Fatal("saved memory on disk must not be discarded by an automatic restart")
	}
}

// writeSavedState leaves a .vzstate on disk, which is what a stale suspension
// orphan looks like to every caller that scans for one.
func writeSavedState(t *testing.T, clusterName string) {
	t.Helper()
	saved := filepath.Join(os.Getenv("HOME"), ".talosbox", "clusters", clusterName)
	if err := os.MkdirAll(saved, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saved, "cp1"+savedStateSuffix), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSavedStateRefusalNamesTheWayOut pins the dead end a stale .vzstate used to
// create: the scan refuses the automatic restart, so the refusal has to say what
// was found and every way past it (#284 review round 3).
func TestSavedStateRefusalNamesTheWayOut(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	stubDaemonRestart(t, fake, unsupervisedDaemon)
	writeSavedState(t, "delta")
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	err := command.call("status", struct{}{}, nil)
	if err == nil {
		t.Fatal("saved memory on disk must not be discarded by an automatic restart")
	}
	for _, want := range []string{
		"delta",
		"tbx cluster resume delta",
		"tbx cluster destroy",
		"tbx system restart --force",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestSystemRestartRefusalNamesTheWayOutOfSavedState(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	stubDaemonRestart(t, fake, unsupervisedDaemon)
	writeSavedState(t, "delta")
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.run([]string{"system", "restart"})
	if err == nil {
		t.Fatal("an unforced restart must not discard saved memory")
	}
	for _, want := range []string{"delta", "tbx cluster resume delta", "tbx system restart --force"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

// TestSystemRestartForceProceedsPastSavedStateOnDisk pins the escape the
// refusal promises: --force must actually get past a disk-detected suspension,
// and must say what that cost.
func TestSystemRestartForceProceedsPastSavedStateOnDisk(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	terminated := stubDaemonRestart(t, fake, unsupervisedDaemon)
	writeSavedState(t, "delta")
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	if err := command.run([]string{"system", "restart", "--force"}); err != nil {
		t.Fatalf("forced restart = %v, want it to proceed past saved state on disk", err)
	}
	if *terminated != 1 {
		t.Fatalf("terminate calls = %d, want 1", *terminated)
	}
	got := stdout.String()
	if !strings.Contains(got, "delta") || !strings.Contains(got, "lose their saved memory") {
		t.Fatalf("stdout = %q, want the discarded saved memory reported", got)
	}
}

// TestGateOnInferredSupervisionNamesTheForcedRestart pins that the gate and the
// restart cannot diverge: `tbx system restart --force` yields on inferred
// supervision, so the gate must name it — the supervisor's own command may not
// exist when the unit file is an orphan.
func TestGateOnInferredSupervisionNamesTheForcedRestart(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	reason := "tbxd may be managed by /usr/lib/systemd/user/tbxd.service"
	terminated := stubDaemonRestart(t, fake, func() (supervision, string) {
		return supervisionInferred, reason
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	err := command.call("status", struct{}{}, nil)
	if err == nil {
		t.Fatal("a stale daemon under an inferred supervisor must fail the gated verb")
	}
	for _, want := range []string{"tbx system restart --force", supervisorRestartCommand(), reason} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
	if *terminated != 0 {
		t.Fatal("a possibly supervised daemon must not be terminated behind a read-only verb")
	}

	// the same refusal text both callers build, so they cannot drift apart
	if !strings.Contains(err.Error(), supervisionRefusal(supervisionInferred, reason)) {
		t.Fatalf("error = %q, want the shared supervision refusal %q", err, supervisionRefusal(supervisionInferred, reason))
	}
	restartErr := cli{out: &stdout, err: &stderr}.run([]string{"system", "restart"})
	if restartErr == nil || !strings.Contains(restartErr.Error(), supervisionRefusal(supervisionInferred, reason)) {
		t.Fatalf("restart error = %v, want the same shared refusal", restartErr)
	}
}

// TestBusyHandshakeRetriesOnceAndWarnsBeforeSkipping pins the busy-skip's cost:
// the gate tries again with a longer deadline, and never skips silently.
func TestBusyHandshakeRetriesOnceAndWarnsBeforeSkipping(t *testing.T) {
	shortenHandshakeTimeout(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	scripted := newScriptedDaemon(t, func(_ int, connection net.Conn, request *daemon.Request) {
		if request != nil && request.Op == "daemon.info" {
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
	case <-time.After(15 * time.Second):
		t.Fatal("a busy daemon hung the protocol gate")
	}

	handshakes := 0
	for _, op := range scripted.recordedOps() {
		if op == "daemon.info" {
			handshakes++
		}
	}
	if handshakes != 2 {
		t.Fatalf("daemon.info attempts = %d, want one retry before skipping", handshakes)
	}
	if got := strings.Count(stderr.String(), unverifiedProtocolNotice); got != 1 {
		t.Fatalf("stderr = %q, want exactly one skip notice", stderr.String())
	}
}

// TestSystemRestartForceReplacesAnInferredSupervisedDaemon pins the dead end
// out of the recovery chain: every packaged install ships a unit file, so a
// unit file alone must not make the restart unavailable.
func TestSystemRestartForceReplacesAnInferredSupervisedDaemon(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	terminated := stubDaemonRestart(t, fake, func() (supervision, string) {
		return supervisionInferred, "tbxd may be managed by /usr/lib/systemd/user/tbxd.service"
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.run([]string{"system", "restart"})
	if err == nil || !strings.Contains(err.Error(), supervisorRestartCommand()) {
		t.Fatalf("error = %v, want a refusal naming the supervisor command", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error = %q, want the force escape named", err)
	}
	if *terminated != 0 {
		t.Fatal("an unforced restart must not replace a possibly supervised daemon")
	}

	if err := command.run([]string{"system", "restart", "--force"}); err != nil {
		t.Fatalf("forced restart = %v, want it to proceed on inferred supervision", err)
	}
	if *terminated != 1 {
		t.Fatalf("terminate calls = %d, want 1", *terminated)
	}
}

// TestSystemRestartForceStillRefusesAConfirmedSupervisor pins the other half:
// when the supervisor answers that it owns an active tbxd, the process is not
// tbx's to kill at any force level.
func TestSystemRestartForceStillRefusesAConfirmedSupervisor(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	terminated := stubDaemonRestart(t, fake, func() (supervision, string) {
		return supervisionConfirmed, "tbxd is managed by systemd (--user unit tbxd.socket)"
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.run([]string{"system", "restart", "--force"})
	if err == nil || !strings.Contains(err.Error(), supervisorRestartCommand()) {
		t.Fatalf("error = %v, want --force refused with the supervisor command", err)
	}
	if *terminated != 0 {
		t.Fatal("a confirmed supervised daemon must never be terminated")
	}
}

func TestCallRefusesToRestartWhenTheSavedStateScanFails(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	terminated := stubDaemonRestart(t, fake, unsupervisedDaemon)
	stubRunningClusters(t, func(string) (clusterActivity, error) { return clusterActivity{}, nil })
	previous := savedStateClustersQuery
	t.Cleanup(func() { savedStateClustersQuery = previous })
	savedStateClustersQuery = func() ([]string, error) { return nil, errors.New("scan failed") }
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	err := command.call("status", struct{}{}, nil)
	if err == nil || !strings.Contains(err.Error(), "scan for suspended clusters") {
		t.Fatalf("error = %v, want a refusal naming the failed suspension scan", err)
	}
	if *terminated != 0 {
		t.Fatal("an unknown suspension state must not be terminated through")
	}
}

func TestFeatureGateMessagesNameTheForcedRestartEscape(t *testing.T) {
	// The per-verb feature gate is the path taken when the connect-time gate
	// was skipped (busy daemon), so its refusal must name the same escapes.
	newFakeDaemon(t, daemon.ProtocolVersion)
	command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	gateErr := command.ensureProtocolAtLeast(daemon.ProtocolVersion+1, "future feature")
	if gateErr == nil {
		t.Fatal("gate passed a daemon older than the required protocol")
	}
	for _, wanted := range []string{"--force", "supervised install"} {
		if !strings.Contains(gateErr.Error(), wanted) {
			t.Fatalf("gate error = %v, want it to name %q", gateErr, wanted)
		}
	}
}

// stubDaemonLock replaces the socket-lock probe, which is the gate that keeps a
// replacement daemon from binding while the old one still owns the socket.
func stubDaemonLock(t *testing.T, free func(socketPath string) bool) {
	t.Helper()
	previous := daemonLockFree
	t.Cleanup(func() { daemonLockFree = previous })
	daemonLockFree = free
}

// shortenDaemonWaits makes the bounded restart waits observable in a test.
func shortenDaemonWaits(t *testing.T, wait, progress time.Duration) {
	t.Helper()
	previousWait, previousProgress, previousBudget := daemonWaitTimeout, daemonExitProgressInterval, machineStopBudget
	t.Cleanup(func() {
		daemonWaitTimeout, daemonExitProgressInterval, machineStopBudget = previousWait, previousProgress, previousBudget
	})
	daemonWaitTimeout, daemonExitProgressInterval, machineStopBudget = wait, progress, 10*time.Millisecond
}

// stubDaemonSpawn wraps the (already stubbed) spawn seam so a test can assert
// when the replacement was launched.
func stubDaemonSpawn(t *testing.T, before func()) *int {
	t.Helper()
	spawned := 0
	previous := spawnDaemonProcess
	t.Cleanup(func() { spawnDaemonProcess = previous })
	spawnDaemonProcess = func() (int64, error) {
		spawned++
		before()
		return previous()
	}
	return &spawned
}

// TestDaemonExitTimeoutScalesWithRunningVMs pins the fix for the 20s give-up:
// the old daemon stops each VM under its own 30s budget, so the wait has to
// grow with the workload rather than stay at the CLI's fixed base (#319).
func TestDaemonExitTimeoutScalesWithRunningVMs(t *testing.T) {
	if got := daemonExitTimeout(clusterActivity{}); got != daemonWaitTimeout {
		t.Fatalf("daemonExitTimeout(idle) = %s, want the base %s", got, daemonWaitTimeout)
	}
	got := daemonExitTimeout(clusterActivity{running: []string{"alpha"}, runningVMs: 3})
	want := daemonWaitTimeout + 3*machineStopBudget
	if got != want {
		t.Fatalf("daemonExitTimeout(3 VMs) = %s, want %s", got, want)
	}
	if want <= daemonWaitTimeout {
		t.Fatal("a running cluster must buy more than the base wait")
	}
}

// TestRunningClustersCountsTheVMsARestartMustStop keeps the scaled wait fed by
// the daemon's own node counts.
func TestRunningClustersCountsTheVMsARestartMustStop(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	fake.runsNodes("alpha", 3, 2)
	fake.suspends("beta")

	activity, err := runningClusters(fake.socket)
	if err != nil {
		t.Fatal(err)
	}
	if activity.runningVMs != 5 {
		t.Fatalf("runningVMs = %d, want 5 (3 control planes + 2 workers)", activity.runningVMs)
	}
}

// TestForcedRestartReportsProgressWhileTheDaemonStopsVMs keeps a long, healthy
// stop from looking hung.
func TestForcedRestartReportsProgressWhileTheDaemonStopsVMs(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	fake.runsNodes("alpha", 1, 2)
	stubDaemonRestart(t, fake, unsupervisedDaemon)
	shortenDaemonWaits(t, 2*time.Second, time.Millisecond)
	stubDaemonLock(t, func(string) bool { return true })
	// stay alive for a few polls so the progress narration has something to say
	previousAlive := daemonProcessAlive
	t.Cleanup(func() { daemonProcessAlive = previousAlive })
	polls := 0
	daemonProcessAlive = func(int) bool {
		polls++
		return polls < 3
	}
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	if err := command.run([]string{"system", "restart", "--force"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"waiting for tbxd", "up to 3 VMs"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
}

// TestForcedRestartFailsWithoutSpawningWhenTheDaemonKeepsStopping pins the
// third part of the fix: a daemon that really will not exit must not be raced
// by a replacement that then dies on the socket lock.
func TestForcedRestartFailsWithoutSpawningWhenTheDaemonKeepsStopping(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	fake.runsNodes("alpha", 1, 0)
	stubDaemonRestart(t, fake, unsupervisedDaemon)
	shortenDaemonWaits(t, 20*time.Millisecond, time.Hour)
	stubDaemonLock(t, func(string) bool { return true })
	previousAlive := daemonProcessAlive
	t.Cleanup(func() { daemonProcessAlive = previousAlive })
	daemonProcessAlive = func(int) bool { return true }
	spawned := stubDaemonSpawn(t, func() {})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.run([]string{"system", "restart", "--force"})
	if err == nil {
		t.Fatal("a daemon that never exits must fail the restart")
	}
	for _, want := range []string{"still stopping", "tbx system restart --force"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to contain %q", err, want)
		}
	}
	if *spawned != 0 {
		t.Fatalf("spawned %d replacements, want none while the old daemon is still up", *spawned)
	}
}

// TestForcedRestartWaitsForTheSocketLockBeforeSpawning pins the race the issue
// reported: the old daemon holds its flock past listener close, so an observed
// process exit is not yet permission to bind (#319).
func TestForcedRestartWaitsForTheSocketLockBeforeSpawning(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	fake.runsNodes("alpha", 1, 0)
	stubDaemonRestart(t, fake, unsupervisedDaemon)
	shortenDaemonWaits(t, 5*time.Second, time.Hour)
	stubHandshakeProbe(t, func(string) (daemon.Info, int, error) {
		return daemon.Info{}, 0, errors.New("no daemon on the socket")
	})
	probes := 0
	free := false
	stubDaemonLock(t, func(socketPath string) bool {
		if socketPath != fake.socket {
			t.Errorf("probed lock for %q, want the daemon socket %q", socketPath, fake.socket)
		}
		probes++
		free = probes > 2
		return free
	})
	spawned := stubDaemonSpawn(t, func() {
		if !free {
			t.Error("the replacement was spawned while the old daemon still owned the socket lock")
		}
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	if err := command.run([]string{"system", "restart", "--force"}); err != nil {
		t.Fatal(err)
	}
	if *spawned != 1 {
		t.Fatalf("spawned %d replacements, want 1", *spawned)
	}
	if probes < 3 {
		t.Fatalf("lock probes = %d, want the spawn to wait for the lock to free", probes)
	}
}

// TestForcedRestartFailsWhenTheSocketLockNeverFrees keeps a stuck lock from
// spawning a replacement that would only die on it.
func TestForcedRestartFailsWhenTheSocketLockNeverFrees(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	stubDaemonRestart(t, fake, unsupervisedDaemon)
	shortenDaemonWaits(t, 20*time.Millisecond, time.Hour)
	stubDaemonLock(t, func(string) bool { return false })
	stubHandshakeProbe(t, func(string) (daemon.Info, int, error) {
		return daemon.Info{}, 0, errors.New("no daemon on the socket")
	})
	spawned := stubDaemonSpawn(t, func() {})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.run([]string{"system", "restart", "--force"})
	if err == nil || !strings.Contains(err.Error(), "still owns") {
		t.Fatalf("error = %v, want a refusal naming the socket the old daemon still owns", err)
	}
	if *spawned != 0 {
		t.Fatalf("spawned %d replacements, want none while the socket lock is held", *spawned)
	}
}

// TestSocketLockFreeReportsAHeldLock exercises the real flock probe, which is
// what the seam stands in for everywhere else.
func TestSocketLockFreeReportsAHeldLock(t *testing.T) {
	home := tempHome(t)
	socket := filepath.Join(home, "tbxd.sock")
	if !socketLockFree(socket) {
		t.Fatal("an unlocked socket must probe as free")
	}
	held, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if socketLockFree(socket) {
		t.Fatal("a held lock must probe as taken")
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if !socketLockFree(socket) {
		t.Fatal("a released lock must probe as free again")
	}
}

// stubHandshakeProbe replaces the lock-wait's liveness probe. The scripted fake
// daemon keeps its listener alive for the whole test, so tests modelling a dead
// old daemon must fail the probe explicitly.
func stubHandshakeProbe(t *testing.T, probe func(string) (daemon.Info, int, error)) {
	t.Helper()
	previous := daemonHandshakeProbe
	t.Cleanup(func() { daemonHandshakeProbe = previous })
	daemonHandshakeProbe = probe
}

func stubOnDiskVMCount(t *testing.T, count int) {
	t.Helper()
	previous := onDiskVMCount
	t.Cleanup(func() { onDiskVMCount = previous })
	onDiskVMCount = func() int { return count }
}

// TestForcedRestartAdoptsAConcurrentlySpawnedDaemon pins the lock-wait's escape
// hatch: a held flock can belong to a fresh daemon another tbx auto-spawned in
// the gap after the old exit, and that is the restart's goal already met — not
// a 20s spin ending in a wrong "still owns" refusal.
func TestForcedRestartAdoptsAConcurrentlySpawnedDaemon(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	fake.runsNodes("alpha", 1, 0)
	stubDaemonRestart(t, fake, unsupervisedDaemon)
	shortenDaemonWaits(t, 5*time.Second, time.Hour)
	stubDaemonLock(t, func(string) bool { return false })
	stubHandshakeProbe(t, func(string) (daemon.Info, int, error) {
		return daemon.Info{ProtocolVersion: daemon.ProtocolVersion}, 4242, nil
	})
	spawned := stubDaemonSpawn(t, func() {})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	if err := command.run([]string{"system", "restart", "--force"}); err != nil {
		t.Fatal(err)
	}
	if *spawned != 0 {
		t.Fatalf("spawned %d replacements, want none when a current daemon already serves the socket", *spawned)
	}
	if got := stdout.String(); !strings.Contains(got, "pid 4242") {
		t.Fatalf("stdout = %q, want it to report the adopted daemon's pid", got)
	}
}

// TestBusyRestartScalesTheWaitFromOnDiskState pins the #319 fallback: the very
// daemon whose shutdown takes minutes is the one too busy to answer
// cluster.list, so the stop-wait must scale from on-disk state instead of
// collapsing to the base timeout.
func TestBusyRestartScalesTheWaitFromOnDiskState(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	stubDaemonRestart(t, fake, unsupervisedDaemon)
	shortenDaemonWaits(t, 20*time.Millisecond, time.Hour)
	stubRunningClusters(t, func(string) (clusterActivity, error) {
		return clusterActivity{}, errors.New("tbxd did not answer cluster.list within 3s")
	})
	stubOnDiskVMCount(t, 3)
	previousAlive := daemonProcessAlive
	t.Cleanup(func() { daemonProcessAlive = previousAlive })
	daemonProcessAlive = func(int) bool { return true }
	spawned := stubDaemonSpawn(t, func() {})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.run([]string{"system", "restart", "--force"})
	if err == nil {
		t.Fatal("a daemon that never exits must fail the restart")
	}
	if !strings.Contains(err.Error(), "up to 3 VMs") {
		t.Fatalf("error = %v, want the wait scaled to the 3 on-disk VMs", err)
	}
	if *spawned != 0 {
		t.Fatalf("spawned %d replacements, want none", *spawned)
	}
}
