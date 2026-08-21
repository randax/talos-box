package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

// A quiet up must still prove it is alive: the blocking provisioning call can
// run for the daemon's full budget, and silence is indistinguishable from a
// hang (#307). The budget it states includes the node boot wait, because an up
// that creates or starts a cluster is held for that too (#364).
func TestUpQuietStatesTheDeadlineAndBeats(t *testing.T) {
	stdout, stderr := runSlowCLI(t, []string{`{"protocolVersion":7}`, `[{"action":"create","cluster":"demo"}]`}, func(command cli) error {
		return command.runUp([]string{"-f", writeUpConfig(t, "demo"), "--quiet"})
	})
	// the file declares a CSI, which buys the wider provisioning budget; an up
	// can also start clusters, whose readiness wait the bound carries too (#364)
	deadline := formatLivenessDuration(startedProvisionDeadline(true))
	if deadline == formatLivenessDuration(provisionDeadline(true)) {
		t.Fatalf("stated deadline %s does not carry the boot wait", deadline)
	}
	for _, wanted := range []string{"provisioning demo", "overall deadline " + deadline, "progress suppressed by --quiet", "still provisioning demo"} {
		if !strings.Contains(stderr, wanted) {
			t.Fatalf("quiet up stderr missing %q:\n%s", wanted, stderr)
		}
	}
	if !strings.Contains(stderr, "elapsed") || !strings.Contains(stderr, "overall deadline "+deadline) {
		t.Fatalf("heartbeat did not carry elapsed/deadline:\n%s", stderr)
	}
	if strings.Contains(stdout, "still provisioning") || strings.Contains(stdout, "overall deadline "+deadline) {
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

// A create is held to the same heartbeat as an up on every phase it waits on,
// including the storage-gated one, and it states the wider budget the daemon
// holds a storage create to (#392).
func TestClusterCreateBeatsOnTheStorageGatedPath(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{name: "cni", args: []string{"demo", "--schematic=test-schematic", "--cni=cilium"}, want: createProvisionDeadline(false)},
		{name: "storage", args: []string{"demo", "--schematic=test-schematic", "--cni=cilium", "--csi=longhorn"}, want: createProvisionDeadline(true)},
	} {
		t.Run(test.name, func(t *testing.T) {
			// --cni/--csi handshake first, then the blocking create.
			responses := []string{`{"protocolVersion":10}`, `{"name":"demo","controlPlanes":1,"workers":2}`}
			_, stderr := runSlowCLI(t, responses, func(command cli) error {
				return command.createCluster(test.args)
			})
			want := "still provisioning demo (elapsed"
			if !strings.Contains(stderr, want) {
				t.Fatalf("create printed no heartbeat:\n%s", stderr)
			}
			if !strings.Contains(stderr, "overall deadline "+formatLivenessDuration(test.want)+")") {
				t.Fatalf("create heartbeat missing the %s bound:\n%s", formatLivenessDuration(test.want), stderr)
			}
		})
	}
}

// A blocking verb states a deadline; without a client-side bound a daemon gate
// that never answers leaves the verb heartbeating forever (#392).
func TestBlockingCallFailsPastItsBound(t *testing.T) {
	previousInterval := livenessInterval
	livenessInterval = 5 * time.Millisecond
	previousGrace := livenessGrace
	livenessGrace = 20 * time.Millisecond
	previousPreamble := livenessPreambleDelay
	livenessPreambleDelay = 5 * time.Millisecond
	t.Cleanup(func() {
		livenessInterval = previousInterval
		livenessGrace = previousGrace
		livenessPreambleDelay = previousPreamble
	})

	home, err := os.MkdirTemp("/tmp", "tbx-bound-")
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

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			var request daemon.Request
			if decodeErr := json.NewDecoder(connection).Decode(&request); decodeErr != nil {
				_ = connection.Close()
				return
			}
			// The gate hangs: the request is never answered.
			go func() {
				<-release
				_ = connection.Close()
			}()
		}
	}()

	var stdout, stderr bytes.Buffer
	signal := liveness{verb: "provisioning demo", deadline: 20 * time.Millisecond, quiet: true}
	err = (cli{out: &stdout, err: &stderr}).callWithLiveness(signal, "cluster.create", map[string]string{"name": "demo"}, nil)
	if err == nil {
		t.Fatal("a hung lifecycle call returned no error")
	}
	for _, wanted := range []string{"tbxd did not finish provisioning demo within", "overall deadline", "tbx status"} {
		if !strings.Contains(err.Error(), wanted) {
			t.Fatalf("bound error = %q, want it to mention %q", err, wanted)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("a hung call wrote to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "still provisioning demo (elapsed") {
		t.Fatalf("a hung call printed no heartbeat while it waited:\n%s", stderr.String())
	}
}

// A start states the bound the daemon actually budgets it at: the stored
// cluster declares no storage engine, so it is the CNI budget plus the boot
// wait a start runs ahead of its reconcile (#307, #364).
func TestClusterStartQuietStatesTheDeadline(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}})
	_, stderr := runSlowCLI(t, []string{`{"name":"demo"}`}, func(command cli) error {
		return command.startCluster([]string{"demo", "--quiet"})
	})
	deadline := formatLivenessDuration(cniProvisionDeadline + nodeBootDeadline + kubernetesReadyDeadline)
	for _, wanted := range []string{"starting demo", "overall deadline " + deadline, "progress suppressed by --quiet", "still starting demo (elapsed"} {
		if !strings.Contains(stderr, wanted) {
			t.Fatalf("quiet start stderr missing %q:\n%s", wanted, stderr)
		}
	}
}

// A run the daemon answers immediately did no provisioning, so --quiet must not
// have opened it by announcing a provisioning window (#421). The preamble is
// left at its production delay: the point is that a no-op never reaches it.
func TestUpQuietNoOpAnnouncesNoProvisioningWindow(t *testing.T) {
	home, requests := startUpTestDaemon(t,
		daemon.Response{OK: true, Data: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%d}`, daemon.ProtocolVersion))},
		daemon.Response{OK: true, Data: json.RawMessage(`[{"cluster":"demo","action":"none"}]`)},
	)
	path := filepath.Join(home, "talosbox.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nclusters:\n  - name: demo\n    cni: cilium\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := (cli{out: &stdout, err: &stderr}).runUp([]string{"-f", path, "--quiet"}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.Op != "daemon.info" {
		t.Fatalf("first operation = %q, want daemon.info", request.Op)
	}
	if request := <-requests; request.Op != "up" {
		t.Fatalf("second operation = %q, want up", request.Op)
	}
	if got := stdout.String(); got != "demo is up to date\n" {
		t.Fatalf("quiet no-op stdout = %q, want the result only", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("quiet no-op announced a provisioning window: %q", stderr.String())
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
	// The quiet preamble waits out the no-op window (#421); a slow call must
	// still reach it well inside the test.
	previousPreamble := livenessPreambleDelay
	livenessPreambleDelay = 5 * time.Millisecond
	t.Cleanup(func() { livenessPreambleDelay = previousPreamble })

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
