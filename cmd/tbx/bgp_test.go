package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// The mode change reconciles Cilium on the request path, so the operator must
// see which half is running instead of watching a silent socket for minutes
// (#273 #344).
func TestBGPEnableNarratesTheDaemonStages(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})

	requests, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":10}`},
		{
			stages: []string{"starting the host BGP speaker for cluster demo", "reconciling cilium on cluster demo (up to 10m)"},
			data:   `{"name":"demo","cni":"cilium","lb":true,"bgp":true}`,
		},
	}, func(command cli) error {
		return command.runBGP([]string{"enable", "demo"})
	})

	if requests[1].Op != "bgp.enable" {
		t.Fatalf("second request op = %q, want bgp.enable", requests[1].Op)
	}
	if !requests[1].Progress {
		t.Fatal("bgp enable did not ask the daemon for progress")
	}
	for _, wanted := range []string{"host BGP speaker for cluster demo", "reconciling cilium on cluster demo"} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("output missing stage %q:\n%s", wanted, output)
		}
	}
	if strings.Index(output, "reconciling cilium") > strings.Index(output, "BGP enabled for cluster demo") {
		t.Fatalf("stages printed after the success line:\n%s", output)
	}
}

func TestBGPDisableQuietSuppressesNarration(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})

	requests, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":10}`},
		{
			stages: []string{"stopping the host BGP speaker for cluster demo"},
			data:   `{"name":"demo","cni":"cilium","lb":true}`,
		},
	}, func(command cli) error {
		return command.runBGP([]string{"disable", "demo", "--quiet"})
	})

	if requests[1].Progress {
		t.Fatal("quiet bgp disable still asked the daemon for progress")
	}
	if strings.Contains(output, "host BGP speaker") {
		t.Fatalf("quiet bgp disable printed narration:\n%s", output)
	}
	if !strings.Contains(output, "BGP disabled for cluster demo") {
		t.Fatalf("quiet bgp disable lost its result:\n%s", output)
	}
}

// Disabling is an announcement-mode flip. The reconcile's equivalent-command
// block is the create-time bootstrap script, and replaying it read as if
// bootstrapping a live cluster were the suggested next step (#400).
func TestBGPDisableOmitsTheCreateStyleEquivalentCommands(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})

	_, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":14}`},
		{
			stages: []string{"stopping the host BGP speaker for cluster demo"},
			data:   `{"name":"demo","cni":"cilium","lb":true,"narration":["bootstrap: ≈ talosctl bootstrap --nodes 172.30.0.68","Cilium chart: ≈ tbx manifests demo objects | kubectl apply --server-side -f -"]}`,
		},
	}, func(command cli) error {
		return command.runBGP([]string{"disable", "demo"})
	})

	if !strings.Contains(output, "stopping the host BGP speaker for cluster demo") {
		t.Fatalf("bgp disable lost its stage narration:\n%s", output)
	}
	if !strings.Contains(output, "BGP disabled for cluster demo") {
		t.Fatalf("bgp disable lost its result:\n%s", output)
	}
	for _, unwanted := range []string{"bootstrap: ≈", "Cilium chart: ≈"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("bgp disable echoed the create-style block %q:\n%s", unwanted, output)
		}
	}
}

// Enabling keeps the block: its equivalent commands are how an operator
// re-applies the announcement objects the mode change just rendered.
func TestBGPEnableKeepsTheEquivalentCommands(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})

	_, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":14}`},
		{data: `{"name":"demo","cni":"cilium","lb":true,"bgp":true,"narration":["Cilium chart: ≈ tbx manifests demo objects | kubectl apply --server-side -f -"]}`},
	}, func(command cli) error {
		return command.runBGP([]string{"enable", "demo"})
	})

	if !strings.Contains(output, "Cilium chart: ≈") {
		t.Fatalf("bgp enable lost its equivalent commands:\n%s", output)
	}
}

// `bgp status` is how an operator confirms a refused or deferred mode change
// without reaching for doctor (#399).
func TestBGPStatusReportsSpeakerStateAndRoutes(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})

	requests, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":14}`},
		{data: `{"name":"demo","cni":"cilium","bgp":true,"speaker":true,"bindAddress":"172.30.0.1","port":179,"routes":[{"prefix":"172.30.0.200/32","nexthop":"172.30.0.2"}]}`},
	}, func(command cli) error {
		return command.runBGP([]string{"status", "demo"})
	})

	if requests[1].Op != "bgp.status" {
		t.Fatalf("second request op = %q, want bgp.status", requests[1].Op)
	}
	for _, wanted := range []string{
		"cluster demo: announcement mode bgp (cni: cilium)",
		"host BGP speaker: running on 172.30.0.1:179",
		"announced route: 172.30.0.200/32 via 172.30.0.2",
	} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("bgp status output missing %q:\n%s", wanted, output)
		}
	}
}

func TestBGPStatusReportsAStoppedSpeaker(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})

	_, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":14}`},
		{data: `{"name":"demo","cni":"flannel","bindAddress":"172.30.0.1","port":179}`},
	}, func(command cli) error {
		return command.runBGP([]string{"status", "demo"})
	})

	for _, wanted := range []string{
		"cluster demo: announcement mode l2 (cni: flannel)",
		"host BGP speaker: stopped",
	} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("bgp status output missing %q:\n%s", wanted, output)
		}
	}
	if strings.Contains(output, "announced route") {
		t.Fatalf("stopped speaker reported routes:\n%s", output)
	}
}

// A deferred reconcile is the one case where the mode is recorded but not in
// effect; the daemon's note has to reach the operator above the success line.
func TestBGPEnablePrintsTheDeferralWarning(t *testing.T) {
	stubStoredClusters(t, daemon.ClusterSummary{Name: "demo"})

	_, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":10}`},
		{data: `{"name":"demo","cni":"cilium","lb":true,"bgp":true,"warnings":["cluster members are stopped; demo is recorded as bgp but Cilium still announces over l2 — start every member to reconcile it"]}`},
	}, func(command cli) error {
		return command.runBGP([]string{"enable", "demo"})
	})

	warning := strings.Index(output, "cluster members are stopped")
	success := strings.Index(output, "BGP enabled for cluster demo")
	if warning < 0 || success < 0 || warning > success {
		t.Fatalf("bgp enable output = %q, want the deferral warning above the success line", output)
	}
}

// An older daemon moves only the host speaker, so it must be refused rather than
// left to report a mode that never took effect (#344).
func TestBGPEnableRefusesADaemonThatOnlyMovesTheHostSpeaker(t *testing.T) {
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
		if request.Op != "daemon.info" {
			t.Errorf("first operation = %q, want daemon.info", request.Op)
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.Info{ProtocolVersion: bgpReconcileProtocolVersion - 1})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"bgp", "enable", "demo"})
	<-done
	if err == nil || !strings.Contains(err.Error(), "too old to use bgp enable") {
		t.Fatalf("bgp enable against protocol %d = %v, want a refusal naming the verb", bgpReconcileProtocolVersion-1, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}
