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

// narratedExchange is one scripted daemon exchange: the stages it streams
// before the result, and the result itself.
type narratedExchange struct {
	stages []string
	data   string
}

// runNarratingCLI serves the exchanges in order over a real socket and returns
// the requests it received plus everything the CLI wrote — stdout and stderr
// into one buffer, so their relative order is observable.
func runNarratingCLI(t *testing.T, exchanges []narratedExchange, run func(cli) error) ([]daemon.Request, string) {
	t.Helper()
	// a t.TempDir() path overruns the unix socket name limit
	home, err := os.MkdirTemp("/tmp", "tbx-narration-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket := filepath.Join(home, ".talosbox", "tbxd.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, listenErr := net.Listen("unix", socket)
	if listenErr != nil {
		t.Fatal(listenErr)
	}
	t.Cleanup(func() { _ = listener.Close() })

	type served struct {
		request daemon.Request
		err     error
	}
	results := make(chan served, len(exchanges))
	go func() {
		for _, exchange := range exchanges {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				results <- served{err: acceptErr}
				return
			}
			var request daemon.Request
			if decodeErr := json.NewDecoder(connection).Decode(&request); decodeErr != nil {
				_ = connection.Close()
				results <- served{err: decodeErr}
				return
			}
			encoder := json.NewEncoder(connection)
			var encodeErr error
			for _, stage := range exchange.stages {
				if err := encoder.Encode(daemon.Response{Stage: stage}); err != nil {
					encodeErr = err
				}
			}
			if err := encoder.Encode(daemon.Response{OK: true, Data: json.RawMessage(exchange.data)}); err != nil {
				encodeErr = err
			}
			_ = connection.Close()
			results <- served{request: request, err: encodeErr}
		}
	}()

	var output bytes.Buffer
	if err := run(cli{out: &output, err: &output}); err != nil {
		t.Fatal(err)
	}
	requests := make([]daemon.Request, 0, len(exchanges))
	for range exchanges {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		requests = append(requests, result.request)
	}
	return requests, output.String()
}

// A snapshot stops the cluster, clones every node disk and restarts it. The
// operator must see that happen instead of a bare success line (#273 #244).
func TestSnapshotCreateNarratesTheDaemonStages(t *testing.T) {
	requests, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":9}`},
		{
			stages: []string{"stopping cluster demo", "cloning 3 node disk(s) as one crash-consistent set", "restarting cluster demo"},
			data:   `{"snapshots":[{"name":"baseline"}]}`,
		},
	}, func(command cli) error {
		return command.snapshotCreate([]string{"demo", "baseline", "--yes"})
	})

	if !requests[1].Progress {
		t.Fatal("snapshot.create did not ask the daemon for progress")
	}
	for _, wanted := range []string{"stopping cluster demo", "cloning 3 node disk(s)", "restarting cluster demo"} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("output missing stage %q:\n%s", wanted, output)
		}
	}
	if strings.Index(output, "restarting cluster demo") > strings.Index(output, "created snapshot baseline") {
		t.Fatalf("stages printed after the success line:\n%s", output)
	}
}

// --quiet drops the narration: the request never asks for it, and a stage that
// arrives anyway is not printed.
func TestSnapshotCreateQuietSuppressesNarration(t *testing.T) {
	requests, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":9}`},
		{stages: []string{"stopping cluster demo"}, data: `{"snapshots":[{"name":"baseline"}]}`},
	}, func(command cli) error {
		return command.snapshotCreate([]string{"demo", "baseline", "--yes", "--quiet"})
	})

	if requests[1].Progress {
		t.Fatal("quiet snapshot.create still asked the daemon for progress")
	}
	if strings.Contains(output, "stopping cluster demo") {
		t.Fatalf("quiet snapshot create printed narration:\n%s", output)
	}
	if !strings.Contains(output, "created snapshot baseline of demo") {
		t.Fatalf("quiet snapshot create lost its result:\n%s", output)
	}
}

func TestSnapshotRestoreQuietSuppressesNarration(t *testing.T) {
	requests, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":9}`},
		{stages: []string{"stopping cluster demo"}, data: `{"snapshots":[{"name":"before"}]}`},
	}, func(command cli) error {
		return command.snapshotRestore([]string{"demo", "before", "--yes", "--quiet"})
	})

	if requests[1].Progress {
		t.Fatal("quiet snapshot.restore still asked the daemon for progress")
	}
	if strings.Contains(output, "stopping cluster demo") {
		t.Fatalf("quiet snapshot restore printed narration:\n%s", output)
	}
}

func TestNodeAddNarratesAndQuietSuppressesIt(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})

	requests, output := runNarratingCLI(t, []narratedExchange{
		{stages: []string{"starting node demo-worker-3"}, data: `{"name":"demo-worker-3"}`},
	}, func(command cli) error {
		return command.runNode([]string{"add", "demo"})
	})
	if !requests[0].Progress || !strings.Contains(output, "starting node demo-worker-3") {
		t.Fatalf("node add did not narrate:\n%s", output)
	}

	requests, output = runNarratingCLI(t, []narratedExchange{
		{stages: []string{"starting node demo-worker-3"}, data: `{"name":"demo-worker-3"}`},
	}, func(command cli) error {
		return command.runNode([]string{"add", "demo", "--quiet"})
	})
	if requests[0].Progress || strings.Contains(output, "starting node demo-worker-3") {
		t.Fatalf("quiet node add narrated:\n%s", output)
	}
}

func TestNodeRemoveQuietSuppressesNarration(t *testing.T) {
	requests, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":9}`},
		{stages: []string{"stopping node demo-worker-2"}, data: `{"name":"demo-worker-2"}`},
	}, func(command cli) error {
		return command.runNode([]string{"remove", "demo", "demo-worker-2", "--quiet"})
	})
	if requests[1].Progress {
		t.Fatal("quiet node.remove still asked the daemon for progress")
	}
	if strings.Contains(output, "stopping node demo-worker-2") {
		t.Fatalf("quiet node remove printed narration:\n%s", output)
	}
}

// The create's warnings describe the cluster its success line claims, so they
// have to print above it — below it they land after the operator has already
// read "created and started" (#263).
func TestClusterCreateNarratesAndPrintsWarningsAboveTheSuccessLine(t *testing.T) {
	requests, output := runNarratingCLI(t, []narratedExchange{
		{
			stages: []string{"preparing the Talos v1.11.2 image", "starting 3 node(s)", "waiting for 3 node(s) to boot", "all 3 node(s) booted"},
			data:   `{"name":"demo","controlPlanes":1,"workers":2,"warnings":["a VPN claims a broad route"]}`,
		},
	}, func(command cli) error {
		return command.createCluster([]string{"demo", "--schematic=test-schematic"})
	})

	if !requests[0].Progress {
		t.Fatal("cluster.create did not ask the daemon for progress")
	}
	success := strings.Index(output, "created and started cluster demo")
	warning := strings.Index(output, "warning: a VPN claims a broad route")
	booted := strings.Index(output, "all 3 node(s) booted")
	if success < 0 || warning < 0 || booted < 0 {
		t.Fatalf("create output missing narration, warning or result:\n%s", output)
	}
	if booted > warning || warning > success {
		t.Fatalf("create printed stages/warnings below its success line:\n%s", output)
	}
}

func TestClusterCreateQuietSuppressesNarration(t *testing.T) {
	requests, output := runNarratingCLI(t, []narratedExchange{
		{stages: []string{"starting 3 node(s)"}, data: `{"name":"demo","controlPlanes":1,"workers":2}`},
	}, func(command cli) error {
		return command.createCluster([]string{"demo", "--schematic=test-schematic", "--quiet"})
	})

	if requests[0].Progress {
		t.Fatal("quiet cluster.create still asked the daemon for progress")
	}
	if strings.Contains(output, "starting 3 node(s)") {
		t.Fatalf("quiet create printed narration:\n%s", output)
	}
	if !strings.Contains(output, "created and started cluster demo") {
		t.Fatalf("quiet create lost its result:\n%s", output)
	}
}
