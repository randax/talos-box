package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// fakeDaemon answers daemon.info with a chosen protocol and records every op it
// is asked to serve, so the connect-time gate can be driven without a real tbxd.
type fakeDaemon struct {
	socket   string
	mu       sync.Mutex
	protocol int
	ops      []string
	listener net.Listener
}

func newFakeDaemon(t *testing.T, protocol int) *fakeDaemon {
	t.Helper()
	home := tempHome(t)
	socket := filepath.Join(home, ".talosbox", "tbxd.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDaemon{socket: socket, protocol: protocol}
	fake.listen(t)
	t.Cleanup(fake.stop)
	return fake
}

func (f *fakeDaemon) listen(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.listener = listener
	f.mu.Unlock()
	go f.serve(listener)
}

func (f *fakeDaemon) serve(listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		var request daemon.Request
		if err := json.NewDecoder(connection).Decode(&request); err == nil {
			_ = json.NewEncoder(connection).Encode(f.respond(request))
		}
		_ = connection.Close()
	}
}

func (f *fakeDaemon) respond(request daemon.Request) daemon.Response {
	f.mu.Lock()
	f.ops = append(f.ops, request.Op)
	protocol := f.protocol
	f.mu.Unlock()
	if request.Op == "daemon.info" {
		data, _ := json.Marshal(daemon.Info{ProtocolVersion: protocol})
		return daemon.Response{OK: true, Data: data}
	}
	return daemon.Response{OK: true, Data: json.RawMessage(`{}`)}
}

func (f *fakeDaemon) stop() {
	f.mu.Lock()
	listener := f.listener
	f.listener = nil
	f.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	_ = os.Remove(f.socket)
}

// relisten stands in for a freshly spawned daemon of the given protocol.
func (f *fakeDaemon) relisten(t *testing.T, protocol int) {
	t.Helper()
	f.mu.Lock()
	f.protocol = protocol
	f.mu.Unlock()
	f.listen(t)
}

func (f *fakeDaemon) recordedOps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// stubDaemonRestart replaces the process-level restart seams with a stop/start
// of the fake daemon, which comes back speaking the CLI's own protocol.
func stubDaemonRestart(t *testing.T, fake *fakeDaemon, supervised func() (bool, string)) *int {
	t.Helper()
	terminated := 0
	previousTerminate, previousSpawn, previousSupervised := terminateDaemonProcess, spawnDaemonProcess, supervisedDaemon
	t.Cleanup(func() {
		terminateDaemonProcess, spawnDaemonProcess, supervisedDaemon = previousTerminate, previousSpawn, previousSupervised
	})
	terminateDaemonProcess = func(int) error {
		terminated++
		fake.stop()
		return nil
	}
	spawnDaemonProcess = func() (int64, error) {
		fake.relisten(t, daemon.ProtocolVersion)
		return 0, nil
	}
	supervisedDaemon = supervised
	return &terminated
}

func unsupervisedDaemon() (bool, string) { return false, "" }

// tempHome keeps the socket path short: unix socket paths are capped well below
// what t.TempDir() produces for long test names.
func tempHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "tbx-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	return home
}

func TestCallRestartsAStaleDaemonTBXOwns(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	terminated := stubDaemonRestart(t, fake, unsupervisedDaemon)
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	if err := command.call("status", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}

	if *terminated != 1 {
		t.Fatalf("terminate calls = %d, want 1", *terminated)
	}
	want := fmt.Sprintf("restarted stale tbxd (protocol %d < %d)", daemon.ProtocolVersion-1, daemon.ProtocolVersion)
	if got := stderr.String(); !strings.Contains(got, want) {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	ops := fake.recordedOps()
	if ops[len(ops)-1] != "status" {
		t.Fatalf("ops = %v, want status served last", ops)
	}
}

func TestCallHandshakesOnlyOncePerSession(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	for range 2 {
		if err := command.call("status", struct{}{}, nil); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"daemon.info", "status", "status"}
	if got := fake.recordedOps(); !slices.Equal(got, want) {
		t.Fatalf("ops = %v, want %v", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestCallFailsEveryVerbWhenAStaleDaemonCannotBeRestarted(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	terminated := stubDaemonRestart(t, fake, func() (bool, string) {
		return true, "tbxd is managed by /Library/LaunchDaemons/dev.talosbox.tbxd.plist"
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	err := command.call("status", struct{}{}, nil)
	if err == nil {
		t.Fatal("a stale daemon that cannot be restarted must fail every verb")
	}
	if !strings.Contains(err.Error(), "run: tbx system restart") {
		t.Fatalf("error = %q, want a real recovery command", err)
	}
	if !strings.Contains(err.Error(), "LaunchDaemons") {
		t.Fatalf("error = %q, want the reason the restart was refused", err)
	}
	if *terminated != 0 {
		t.Fatal("a supervised daemon must not be terminated")
	}
	if got := fake.recordedOps(); slices.Contains(got, "status") {
		t.Fatalf("ops = %v, want the gated verb withheld", got)
	}
	// the gate is remembered, so a second verb fails the same way without redialing
	if second := command.call("cluster.list", struct{}{}, nil); second == nil || second.Error() != err.Error() {
		t.Fatalf("second verb error = %v, want %v", second, err)
	}
}

func TestCallFailsWhenTheDaemonIsNewerThanTBX(t *testing.T) {
	newFakeDaemon(t, daemon.ProtocolVersion+1)
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	err := command.call("status", struct{}{}, nil)
	if err == nil || !strings.Contains(err.Error(), "upgrade tbx") {
		t.Fatalf("error = %v, want an upgrade-tbx failure", err)
	}
}

func TestProtocolGateErrorNamesTheRecoveryCommand(t *testing.T) {
	newFakeDaemon(t, snapshotCreateWarningProtocolVersion-1)
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.ensureSnapshotCreateSupport()
	if err == nil {
		t.Fatal("a too-old daemon must fail the gated verb")
	}
	if !strings.Contains(err.Error(), "run: tbx system restart") {
		t.Fatalf("error = %q, want a real recovery command", err)
	}
	if strings.Contains(err.Error(), "restart or upgrade tbxd") {
		t.Fatalf("error = %q, want no prose recovery advice", err)
	}
}

func TestSystemRestartRestartsTheDaemon(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion-1)
	terminated := stubDaemonRestart(t, fake, unsupervisedDaemon)
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	if err := command.run([]string{"system", "restart"}); err != nil {
		t.Fatal(err)
	}

	if *terminated != 1 {
		t.Fatalf("terminate calls = %d, want 1", *terminated)
	}
	if got := stdout.String(); !strings.Contains(got, "restarted tbxd") ||
		!strings.Contains(got, fmt.Sprintf("protocol %d", daemon.ProtocolVersion)) {
		t.Fatalf("stdout = %q, want the new daemon's pid and protocol", got)
	}
}

func TestSystemRestartStartsADaemonThatIsNotRunning(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	stubDaemonRestart(t, fake, unsupervisedDaemon)
	fake.stop()
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	if err := command.run([]string{"system", "restart"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "started tbxd") {
		t.Fatalf("stdout = %q, want a start confirmation", got)
	}
}

func TestSystemRestartRefusesASupervisedDaemon(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	stubDaemonRestart(t, fake, func() (bool, string) {
		return true, "tbxd is managed by /Library/LaunchDaemons/dev.talosbox.tbxd.plist"
	})
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.run([]string{"system", "restart"})
	if err == nil || !strings.Contains(err.Error(), "LaunchDaemons") {
		t.Fatalf("error = %v, want a refusal naming the supervisor", err)
	}
}

func TestSystemStatusPrintsPIDAndProtocol(t *testing.T) {
	newFakeDaemon(t, daemon.ProtocolVersion-1)
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	if err := command.run([]string{"system", "status"}); err != nil {
		t.Fatal(err)
	}

	got := stdout.String()
	if !strings.Contains(got, fmt.Sprintf("pid %d", os.Getpid())) {
		t.Fatalf("stdout = %q, want the daemon pid", got)
	}
	if !strings.Contains(got, fmt.Sprintf("protocol %d", daemon.ProtocolVersion-1)) {
		t.Fatalf("stdout = %q, want the daemon protocol", got)
	}
	if !strings.Contains(got, fmt.Sprintf("tbx protocol %d", daemon.ProtocolVersion)) {
		t.Fatalf("stdout = %q, want the CLI protocol", got)
	}
	if !strings.Contains(stderr.String(), "run: tbx system restart") {
		t.Fatalf("stderr = %q, want a skew warning naming the recovery command", stderr.String())
	}
}

func TestSystemStatusReportsAStoppedDaemon(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	fake.stop()
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	if err := command.run([]string{"system", "status"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "not running") {
		t.Fatalf("stdout = %q, want a not-running report", got)
	}
}

func TestSystemUsageAndHelpListRestartAndStatus(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}

	err := command.runSystem(nil)
	if err == nil || !strings.Contains(err.Error(), "install|uninstall|restart|status") {
		t.Fatalf("usage = %v, want restart and status", err)
	}
	command.printHelp(&stdout)
	if got := stdout.String(); !strings.Contains(got, "system install|uninstall|restart|status") {
		t.Fatalf("help = %q, want restart and status", got)
	}
}

func TestHandshakeWithoutADaemonLeavesTheSpawnPathAlone(t *testing.T) {
	fake := newFakeDaemon(t, daemon.ProtocolVersion)
	fake.stop()
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, daemon: newDaemonSession()}

	if err := command.ensureDaemonProtocol(); err != nil {
		t.Fatalf("handshake without a daemon = %v, want nil", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestDaemonHandshakeReportsUnknownOperationAsProtocolZero(t *testing.T) {
	home := tempHome(t)
	socket := filepath.Join(home, ".talosbox", "tbxd.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			var request daemon.Request
			if err := json.NewDecoder(connection).Decode(&request); err == nil {
				_ = json.NewEncoder(connection).Encode(daemon.Response{Error: `unknown operation "daemon.info"`})
			}
			_ = connection.Close()
		}
	}()

	info, pid, err := daemonHandshake(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != 0 {
		t.Fatalf("protocol = %d, want 0", info.ProtocolVersion)
	}
	if pid != os.Getpid() {
		t.Fatalf("pid = %d, want %d", pid, os.Getpid())
	}
}

func TestSupervisedDaemonDetectsAUnitFile(t *testing.T) {
	home := tempHome(t)
	unit := filepath.Join(home, "Library", "LaunchAgents", "dev.talosbox.tbxd.plist")
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}

	supervised, reason := supervisedDaemon()
	if !supervised || !strings.Contains(reason, unit) {
		t.Fatalf("supervisedDaemon() = %v, %q, want the unit path", supervised, reason)
	}
}
