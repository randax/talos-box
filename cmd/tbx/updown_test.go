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

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/daemon"
)

func TestLoadUpConfigFileAcceptsForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "talosbox.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nclusters:\n  - name: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, force, quiet, err := loadUpConfigFile([]string{"-f", path, "--force"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].Name != "demo" || !force || quiet {
		t.Fatalf("loadUpConfigFile() = clusters %+v, force %v, quiet %v; want demo, true, false", cfg.Clusters, force, quiet)
	}
}

func TestStrongestProvisioningIntentRequiresCSIProtocolRegardlessOfOrder(t *testing.T) {
	cfg := config.Config{Clusters: []config.ClusterSpec{
		{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true}},
		{ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath, LB: true}},
	}}
	input, ok := strongestProvisioningIntent(cfg)
	if !ok {
		t.Fatal("strongestProvisioningIntent() found no provisioning intent")
	}
	if input.CSI != string(cluster.CSILocalPath) {
		t.Fatalf("strongest provisioning input = %+v, want CSI-bearing cluster", input)
	}
	if got := minimumProvisioningIntentProtocol(input); got != csiProvisioningIntentProtocolVersion {
		t.Fatalf("mixed config minimum protocol = %d, want %d", got, csiProvisioningIntentProtocolVersion)
	}
}

func TestRunUpSendsCSIIntentAfterProtocolHandshake(t *testing.T) {
	home, requests := startUpTestDaemon(t,
		daemon.Response{OK: true, Data: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%d}`, daemon.ProtocolVersion))},
		daemon.Response{OK: true, Data: json.RawMessage(`[]`)},
	)
	path := filepath.Join(home, "talosbox.yaml")
	contents := "version: 1\nclusters:\n  - name: demo\n    cni: cilium\n    csi: local-path\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.runUp([]string{"-f", path}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.Op != "daemon.info" {
		t.Fatalf("first operation = %q, want daemon.info", request.Op)
	}
	request := <-requests
	if request.Op != "up" {
		t.Fatalf("second operation = %q, want up", request.Op)
	}
	if !strings.Contains(string(request.Args), `"csi":"local-path"`) {
		t.Fatalf("up request missing CSI intent: %s", request.Args)
	}
}

func TestRunUpRejectsProtocolOneWhenLaterClusterUsesCSI(t *testing.T) {
	home, requests := startUpTestDaemon(t,
		daemon.Response{OK: true, Data: json.RawMessage(`{"protocolVersion":1}`)},
	)
	path := filepath.Join(home, "talosbox.yaml")
	contents := `version: 1
clusters:
  - name: networking
    cni: cilium
  - name: storage
    cni: flannel
    csi: local-path
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := (cli{out: &stdout, err: &stderr}).runUp([]string{"-f", path})
	if err == nil || !strings.Contains(err.Error(), "tbxd protocol 1 is too old") {
		t.Fatalf("runUp() error = %v, want protocol-1 CSI refusal", err)
	}
	if request := <-requests; request.Op != "daemon.info" {
		t.Fatalf("operation = %q, want daemon.info before any mutation", request.Op)
	}
}

func TestRunUpQuietSuppressesNarrationButDefaultOutputShowsManualEquivalents(t *testing.T) {
	t.Run("default output includes stage narration", func(t *testing.T) {
		home, requests := startUpTestDaemon(t,
			daemon.Response{OK: true, Data: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%d}`, daemon.ProtocolVersion))},
			daemon.Response{OK: true, Data: json.RawMessage(`[{"cluster":"demo","action":"reconcile","narration":["machine config: ≈ talosctl apply-config --insecure --nodes 172.30.0.2 --file controlplane.yaml","Cilium chart: ≈ helm template cilium cilium/cilium --version 1.19.6 -n kube-system | kubectl apply --server-side -f -"]}]`)},
		)
		path := filepath.Join(home, "talosbox.yaml")
		if err := os.WriteFile(path, []byte("version: 1\nclusters:\n  - name: demo\n    cni: cilium\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		if err := (cli{out: &stdout, err: &stderr}).runUp([]string{"-f", path}); err != nil {
			t.Fatal(err)
		}
		if request := <-requests; request.Op != "daemon.info" {
			t.Fatalf("first operation = %q, want daemon.info", request.Op)
		}
		if request := <-requests; request.Op != "up" {
			t.Fatalf("second operation = %q, want up", request.Op)
		}
		for _, want := range []string{
			"reconciled demo\n",
			"machine config: ≈ talosctl apply-config --insecure --nodes 172.30.0.2 --file controlplane.yaml",
			"Cilium chart: ≈ helm template cilium cilium/cilium --version 1.19.6 -n kube-system | kubectl apply --server-side -f -",
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("default runUp output missing %q:\n%s", want, stdout.String())
			}
		}
	})

	t.Run("quiet suppresses stage narration", func(t *testing.T) {
		home, requests := startUpTestDaemon(t,
			daemon.Response{OK: true, Data: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%d}`, daemon.ProtocolVersion))},
			daemon.Response{OK: true, Data: json.RawMessage(`[{"cluster":"demo","action":"reconcile","narration":["machine config: ≈ talosctl apply-config --insecure --nodes 172.30.0.2 --file controlplane.yaml","Cilium chart: ≈ helm template cilium cilium/cilium --version 1.19.6 -n kube-system | kubectl apply --server-side -f -"]}]`)},
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
		if got := stdout.String(); got != "reconciled demo\n" {
			t.Fatalf("quiet runUp output = %q, want final action only", got)
		}
	})
}

func startUpTestDaemon(t *testing.T, responses ...daemon.Response) (string, <-chan daemon.Request) {
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
	return home, requests
}

func TestPrintActionsWritesWarningsToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.printActions(
		[]daemon.Action{{Cluster: "demo", Kind: daemon.ActionStart, Warning: "host pressure (forced)"}},
		map[daemon.ActionKind]string{daemon.ActionStart: "started %s"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "started demo\n" {
		t.Fatalf("stdout = %q, want start action", got)
	}
	if got := stderr.String(); !strings.Contains(got, "warning: host pressure (forced)") {
		t.Fatalf("stderr = %q, want host-pressure warning", got)
	}
}

func TestPrintActionsNamesIncompleteProvisioningAsReconciliation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.printActions(
		[]daemon.Action{{Cluster: "demo", Kind: daemon.ActionReconcile}},
		map[daemon.ActionKind]string{daemon.ActionReconcile: "reconciled %s", daemon.ActionNone: "%s is up to date"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "reconciled demo\n" {
		t.Fatalf("stdout = %q, want reconciliation instead of up-to-date", got)
	}
}

func TestPrintActionsQuietKeepsFinalOutputButSuppressesNarration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.printActions(
		[]daemon.Action{{Cluster: "demo", Kind: daemon.ActionCreate, Narration: []string{"≈ talosctl apply-config"}}},
		map[daemon.ActionKind]string{daemon.ActionCreate: "created %s"}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "created demo\n" {
		t.Fatalf("quiet output = %q, want final action only", got)
	}
}

func TestRunUpSendsPerClusterTalosAfterProtocolHandshake(t *testing.T) {
	home, requests := startUpTestDaemon(t,
		daemon.Response{OK: true, Data: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%d}`, daemon.ProtocolVersion))},
		daemon.Response{OK: true, Data: json.RawMessage(`[]`)},
	)
	path := filepath.Join(home, "talosbox.yaml")
	contents := `version: 1
talos:
  version: v1.13.6
clusters:
  - name: stable
  - name: canary
    talos:
      version: v1.14.0
      schematic: bbb
      extensions: []
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := (cli{out: &stdout, err: &stderr}).runUp([]string{"-f", path}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.Op != "daemon.info" {
		t.Fatalf("first operation = %q, want daemon.info handshake", request.Op)
	}
	request := <-requests
	if request.Op != "up" {
		t.Fatalf("second operation = %q, want up", request.Op)
	}
	var args struct {
		Talos    config.TalosSpec
		Clusters []config.ClusterSpec
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if got := args.Clusters[0].Talos; got.Version != "v1.13.6" || got.Schematic != "" {
		t.Fatalf("stable talos on the wire = %#v, want inherited v1.13.6", got)
	}
	if got := args.Clusters[1].Talos; got.Version != "v1.14.0" || got.Schematic != "bbb" {
		t.Fatalf("canary talos on the wire = %#v, want the override", got)
	}
	if args.Clusters[1].Talos.Extensions == nil || len(args.Clusters[1].Talos.Extensions) != 0 {
		t.Fatalf("canary extensions opt-out lost on the wire: %#v", args.Clusters[1].Talos.Extensions)
	}
	if args.Talos.Version != "v1.13.6" {
		t.Fatalf("file-level talos = %#v, want it kept for older daemons", args.Talos)
	}
}

func TestRunUpRejectsOldDaemonWhenPerClusterTalosIsUsed(t *testing.T) {
	home, requests := startUpTestDaemon(t,
		daemon.Response{OK: true, Data: json.RawMessage(`{"protocolVersion":4}`)},
	)
	path := filepath.Join(home, "talosbox.yaml")
	contents := "version: 1\ntalos:\n  version: v1.13.6\nclusters:\n  - name: demo\n    talos:\n      version: v1.14.0\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := (cli{out: &stdout, err: &stderr}).runUp([]string{"-f", path})
	if err == nil || !strings.Contains(err.Error(), "tbxd protocol 4 is too old") {
		t.Fatalf("runUp() error = %v, want per-cluster talos refusal", err)
	}
	if request := <-requests; request.Op != "daemon.info" {
		t.Fatalf("operation = %q, want daemon.info before any mutation", request.Op)
	}
}

func TestRunUpSkipsHandshakeWithoutPerClusterTalosOverrides(t *testing.T) {
	// A file-level talos block alone is understood by every daemon version;
	// only a per-cluster divergence needs the protocol handshake.
	home, requests := startUpTestDaemon(t,
		daemon.Response{OK: true, Data: json.RawMessage(`[]`)},
	)
	path := filepath.Join(home, "talosbox.yaml")
	contents := "version: 1\ntalos:\n  version: v1.13.6\nclusters:\n  - name: demo\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := (cli{out: &stdout, err: &stderr}).runUp([]string{"-f", path}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.Op != "up" {
		t.Fatalf("first operation = %q, want up without a handshake", request.Op)
	}
}

func TestRunUpRejectsOldDaemonWhenFileLevelExtensionsAreUsed(t *testing.T) {
	// The extensions field is new at protocol 5; even a purely inherited
	// file-level list would be silently dropped by an older daemon.
	home, requests := startUpTestDaemon(t,
		daemon.Response{OK: true, Data: json.RawMessage(`{"protocolVersion":4}`)},
	)
	path := filepath.Join(home, "talosbox.yaml")
	contents := "version: 1\ntalos:\n  extensions: [gvisor]\nclusters:\n  - name: demo\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := (cli{out: &stdout, err: &stderr}).runUp([]string{"-f", path})
	if err == nil || !strings.Contains(err.Error(), "tbxd protocol 4 is too old") {
		t.Fatalf("runUp() error = %v, want per-cluster talos refusal", err)
	}
	if request := <-requests; request.Op != "daemon.info" {
		t.Fatalf("operation = %q, want daemon.info before any mutation", request.Op)
	}
}
