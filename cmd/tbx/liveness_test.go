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

// A quiet up must still prove it is alive: the blocking provisioning call can
// run for the daemon's full budget, and silence is indistinguishable from a
// hang (#307).
func TestUpQuietStatesTheDeadlineAndBeats(t *testing.T) {
	stdout, stderr := runSlowCLI(t, []string{`{"protocolVersion":7}`, `[{"action":"create","cluster":"demo"}]`}, func(command cli) error {
		return command.runUp([]string{"-f", writeUpConfig(t, "demo"), "--quiet"})
	})
	for _, wanted := range []string{"provisioning demo", "up to 25m", "progress suppressed by --quiet", "still provisioning demo"} {
		if !strings.Contains(stderr, wanted) {
			t.Fatalf("quiet up stderr missing %q:\n%s", wanted, stderr)
		}
	}
	if !strings.Contains(stderr, "elapsed") || !strings.Contains(stderr, "deadline 25m") {
		t.Fatalf("heartbeat did not carry elapsed/deadline:\n%s", stderr)
	}
	if strings.Contains(stdout, "still provisioning") || strings.Contains(stdout, "up to 25m") {
		t.Fatalf("liveness leaked into stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "created demo") {
		t.Fatalf("quiet up lost its final result:\n%s", stdout)
	}
}

// Without --quiet the deadline preamble would be noise, but the heartbeat is
// still the only sign of life during the blocking call.
func TestUpBeatsWithoutQuietButStatesNoPreamble(t *testing.T) {
	_, stderr := runSlowCLI(t, []string{`{"protocolVersion":7}`, `[{"action":"create","cluster":"demo"}]`}, func(command cli) error {
		return command.runUp([]string{"-f", writeUpConfig(t, "demo")})
	})
	if strings.Contains(stderr, "progress suppressed by --quiet") {
		t.Fatalf("loud up printed the quiet preamble:\n%s", stderr)
	}
	if !strings.Contains(stderr, "still provisioning demo") {
		t.Fatalf("loud up printed no heartbeat:\n%s", stderr)
	}
}

func TestClusterCreateQuietStatesTheDeadline(t *testing.T) {
	_, stderr := runSlowCLI(t, []string{`{"name":"demo","controlPlanes":1,"workers":2}`}, func(command cli) error {
		return command.createCluster([]string{"demo", "--schematic=test-schematic", "--quiet"})
	})
	for _, wanted := range []string{"provisioning demo", "progress suppressed by --quiet", "still provisioning demo"} {
		if !strings.Contains(stderr, wanted) {
			t.Fatalf("quiet create stderr missing %q:\n%s", wanted, stderr)
		}
	}
}

func TestClusterStartQuietStatesTheDeadline(t *testing.T) {
	_, stderr := runSlowCLI(t, []string{`{"name":"demo"}`}, func(command cli) error {
		return command.startCluster([]string{"demo", "--quiet"})
	})
	for _, wanted := range []string{"starting demo", "progress suppressed by --quiet", "still starting demo"} {
		if !strings.Contains(stderr, wanted) {
			t.Fatalf("quiet start stderr missing %q:\n%s", wanted, stderr)
		}
	}
}

func TestFormatLivenessDuration(t *testing.T) {
	for _, testCase := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m"},
		{25 * time.Minute, "25m"},
		{90 * time.Minute, "1h30m"},
	} {
		if got := formatLivenessDuration(testCase.in); got != testCase.want {
			t.Fatalf("formatLivenessDuration(%s) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}

func writeUpConfig(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "talosbox.yaml")
	content := "version: 1\nclusters:\n  - name: " + name + "\n    controlPlanes: 1\n    workers: 1\n    cni: cilium\n    csi: local-path\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runSlowCLI serves one scripted response after a delay that outlasts a few
// heartbeat intervals, so the ticker is exercised without a slow test.
func runSlowCLI(t *testing.T, responses []string, run func(cli) error) (string, string) {
	t.Helper()
	previousInterval := livenessInterval
	livenessInterval = 5 * time.Millisecond
	t.Cleanup(func() { livenessInterval = previousInterval })

	// A unix socket path is short; t.TempDir() names are not.
	home, err := os.MkdirTemp("/tmp", "tbx-liveness-")
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

	served := make(chan error, len(responses))
	go func() {
		for index, response := range responses {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				served <- acceptErr
				return
			}
			var request daemon.Request
			if decodeErr := json.NewDecoder(connection).Decode(&request); decodeErr != nil {
				_ = connection.Close()
				served <- decodeErr
				return
			}
			// Only the operation under test is slow; the handshake ahead of it
			// answers at once.
			if index == len(responses)-1 {
				time.Sleep(60 * time.Millisecond)
			}
			encodeErr := json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: json.RawMessage(response)})
			_ = connection.Close()
			served <- encodeErr
		}
	}()

	var stdout, stderr bytes.Buffer
	if err := run(cli{out: &stdout, err: &stderr}); err != nil {
		t.Fatal(err)
	}
	for range responses {
		if err := <-served; err != nil {
			t.Fatal(err)
		}
	}
	return stdout.String(), stderr.String()
}
