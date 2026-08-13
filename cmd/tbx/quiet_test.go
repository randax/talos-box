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

func TestCreateQuietSuppressesProvisioningNarration(t *testing.T) {
	stdout := runCLIWithResponses(t, []string{
		`{"protocolVersion":1}`,
		`{"name":"demo","controlPlanes":1,"workers":2,"nodeDefaults":{"memoryMiB":2048,"cpus":2,"diskGiB":20},"cni":"flannel","lb":false,"narration":["machine config: ≈ talosctl apply-config"]}`,
	},
		func(command cli) error {
			return command.createCluster([]string{"demo", "--cni=flannel", "--lb=false", "--quiet"})
		},
	)
	if strings.Contains(stdout, "≈ talosctl") {
		t.Fatalf("quiet create printed narration:\n%s", stdout)
	}
	if !strings.Contains(stdout, "created and started cluster demo") || !strings.Contains(stdout, "cni: flannel") {
		t.Fatalf("quiet create hid final output:\n%s", stdout)
	}
}

func TestStartQuietSuppressesProvisioningNarration(t *testing.T) {
	stdout := runCLIWithResponse(t,
		`{"name":"demo","narration":["Cilium chart: ≈ helm template"]}`,
		func(command cli) error { return command.startCluster([]string{"demo", "--quiet"}) },
	)
	if stdout != "started cluster demo\n" {
		t.Fatalf("quiet start output = %q, want final result only", stdout)
	}
}

func runCLIWithResponse(t *testing.T, data string, run func(cli) error) string {
	return runCLIWithResponses(t, []string{data}, run)
}

func runCLIWithResponses(t *testing.T, data []string, run func(cli) error) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "tbx-quiet-")
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

	served := make(chan error, len(data))
	go func() {
		for _, responseData := range data {
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
			encodeErr := json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: json.RawMessage(responseData)})
			_ = connection.Close()
			served <- encodeErr
		}
	}()

	var stdout, stderr bytes.Buffer
	if err := run(cli{out: &stdout, err: &stderr}); err != nil {
		t.Fatal(err)
	}
	for range data {
		if err := <-served; err != nil {
			t.Fatal(err)
		}
	}
	return stdout.String()
}
