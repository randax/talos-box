package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

func TestCreateClusterRejectsInvalidProvisioningFlagsBeforeDaemonCall(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.createCluster([]string{"demo", "--lb=false"})
	if err == nil || !strings.Contains(err.Error(), "lb requires cni") {
		t.Fatalf("createCluster() error = %v, want CNI validation error", err)
	}
}

func TestMinimumProvisioningIntentProtocolPreservesLegacyCNICompatibility(t *testing.T) {
	if got := minimumProvisioningIntentProtocol(cluster.ProvisioningIntentInput{CNI: "cilium"}); got != 1 {
		t.Fatalf("CNI-only minimum protocol = %d, want 1", got)
	}
	if got := minimumProvisioningIntentProtocol(cluster.ProvisioningIntentInput{CNI: "cilium", CSI: "longhorn"}); got != daemon.ProtocolVersion {
		t.Fatalf("CSI minimum protocol = %d, want %d", got, daemon.ProtocolVersion)
	}
}

func TestCreateClusterRejectsCSIWithoutCNIBeforeDaemonCall(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.createCluster([]string{"demo", "--csi=longhorn"})
	if err == nil {
		t.Fatal("createCluster() error = nil, want CSI validation error")
	}
	for _, part := range []string{"csi requires cni", "add cni:", "install storage yourself from the printed manifests"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("createCluster() error = %v, want containing %q", err, part)
		}
	}
}

func TestCreateClusterRejectsUnknownCSIBeforeDaemonCall(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.createCluster([]string{"demo", "--cni=cilium", "--csi=rook"})
	if err == nil || !strings.Contains(err.Error(), "csi must be one of longhorn | local-path") {
		t.Fatalf("createCluster() error = %v, want curated CSI validation error", err)
	}
}

func TestCreateClusterRejectsFlannelBGPBeforeDaemonCall(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.createCluster([]string{"demo", "--cni=flannel", "--bgp"})
	if err == nil || !strings.Contains(err.Error(), "bgp requires cni: cilium") {
		t.Fatalf("createCluster() error = %v, want CNI validation error", err)
	}
}

func TestCreateClusterSendsProvisioningIntentAndPrintsIt(t *testing.T) {
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

	requests := make(chan daemon.Request, 2)
	go func() {
		acceptAndRespond := func(response daemon.Response) bool {
			connection, err := listener.Accept()
			if err != nil {
				return false
			}
			defer func() { _ = connection.Close() }()
			var request daemon.Request
			if err := json.NewDecoder(connection).Decode(&request); err != nil {
				return false
			}
			requests <- request
			return json.NewEncoder(connection).Encode(response) == nil
		}
		if !acceptAndRespond(daemon.Response{OK: true, Data: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%d}`, daemon.ProtocolVersion))}) {
			return
		}
		_ = acceptAndRespond(daemon.Response{OK: true, Data: json.RawMessage(`{"name":"demo","controlPlanes":1,"workers":2,"nodeDefaults":{"memoryMiB":2048,"cpus":2,"diskGiB":20},"cni":"cilium","csi":"longhorn","lb":true,"bgp":true,"hubble":true}`)})
	}()

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.createCluster([]string{"demo", "--schematic=test-schematic", "--cni=cilium", "--csi=longhorn", "--bgp", "--hubble"}); err != nil {
		t.Fatal(err)
	}
	infoRequest := <-requests
	if infoRequest.Op != "daemon.info" {
		t.Fatalf("first operation = %q, want daemon.info", infoRequest.Op)
	}
	request := <-requests
	if request.Op != "cluster.create" {
		t.Fatalf("second operation = %q, want cluster.create", request.Op)
	}
	var args struct {
		CNI    cluster.CNI `json:"cni"`
		CSI    cluster.CSI `json:"csi"`
		LB     *bool       `json:"lb"`
		BGP    *bool       `json:"bgp"`
		Hubble *bool       `json:"hubble"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.CNI != cluster.CNICilium || args.CSI != cluster.CSILonghorn || args.LB == nil || !*args.LB || args.BGP == nil || !*args.BGP || args.Hubble == nil || !*args.Hubble {
		t.Fatalf("create arguments = %+v, want cilium defaults and requested features", args)
	}
	for _, line := range []string{"cni: cilium", "csi: longhorn", "lb: true", "bgp: true", "hubble: true"} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("output missing %q:\n%s", line, stdout.String())
		}
	}
}

func TestCreateClusterDefaultsFlannelLoadBalancerOn(t *testing.T) {
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

	requests := make(chan daemon.Request, 2)
	go func() {
		acceptAndRespond := func(response daemon.Response) bool {
			connection, err := listener.Accept()
			if err != nil {
				return false
			}
			defer func() { _ = connection.Close() }()
			var request daemon.Request
			if err := json.NewDecoder(connection).Decode(&request); err != nil {
				return false
			}
			requests <- request
			return json.NewEncoder(connection).Encode(response) == nil
		}
		if !acceptAndRespond(daemon.Response{OK: true, Data: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%d}`, daemon.ProtocolVersion))}) {
			return
		}
		_ = acceptAndRespond(daemon.Response{OK: true, Data: json.RawMessage(`{"name":"demo","controlPlanes":1,"workers":2,"nodeDefaults":{"memoryMiB":2048,"cpus":2,"diskGiB":20},"cni":"flannel","lb":true}`)})
	}()

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.createCluster([]string{"demo", "--schematic=test-schematic", "--cni=flannel"}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.Op != "daemon.info" {
		t.Fatalf("first operation = %q, want daemon.info", request.Op)
	}
	request := <-requests
	if request.Op != "cluster.create" {
		t.Fatalf("second operation = %q, want cluster.create", request.Op)
	}
	var args struct {
		CNI cluster.CNI `json:"cni"`
		LB  *bool       `json:"lb"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.CNI != cluster.CNIFlannel || args.LB == nil || !*args.LB {
		t.Fatalf("create arguments = %+v, want flannel with lb defaulted on", args)
	}
	for _, line := range []string{"cni: flannel", "lb: true"} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("output missing %q:\n%s", line, stdout.String())
		}
	}
}

func TestCreateClusterRejectsOldDaemonBeforeMutation(t *testing.T) {
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

	requests := make(chan daemon.Request, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		var request daemon.Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			return
		}
		requests <- request
		_ = json.NewEncoder(connection).Encode(daemon.Response{OK: false, Error: `unknown operation "daemon.info"`})
	}()

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err = command.createCluster([]string{"demo", "--cni=cilium", "--bgp"})
	if err == nil || !strings.Contains(err.Error(), "tbxd is too old") {
		t.Fatalf("createCluster() error = %v, want tbxd upgrade refusal", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty output after preflight refusal", stdout.String())
	}
	request := <-requests
	if request.Op != "daemon.info" {
		t.Fatalf("operation = %q, want daemon.info", request.Op)
	}
	<-serverDone
}

func TestCreateClusterWithCSIRejectsOldDaemonBeforeMutation(t *testing.T) {
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

	requests := make(chan daemon.Request, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		var request daemon.Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			return
		}
		requests <- request
		response := fmt.Sprintf(`{"protocolVersion":%d}`, daemon.ProtocolVersion-1)
		_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: json.RawMessage(response)})
	}()

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err = command.createCluster([]string{"demo", "--cni=cilium", "--csi=longhorn"})
	if err == nil || !strings.Contains(err.Error(), "tbxd protocol 1 is too old") || !strings.Contains(err.Error(), "--csi") {
		t.Fatalf("createCluster() error = %v, want CSI protocol upgrade refusal", err)
	}
	if request := <-requests; request.Op != "daemon.info" {
		t.Fatalf("operation = %q, want daemon.info", request.Op)
	}
}

func TestCreateClusterRejectsNewDaemonBeforeMutation(t *testing.T) {
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

	requests := make(chan daemon.Request, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		var request daemon.Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			return
		}
		requests <- request
		response := fmt.Sprintf(`{"protocolVersion":%d}`, daemon.ProtocolVersion+1)
		_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: json.RawMessage(response)})
	}()

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err = command.createCluster([]string{"demo", "--cni=cilium", "--bgp"})
	wantError := fmt.Sprintf("tbx is too old: protocol %d does not support tbxd protocol %d", daemon.ProtocolVersion, daemon.ProtocolVersion+1)
	if err == nil || !strings.Contains(err.Error(), wantError) {
		t.Fatalf("createCluster() error = %v, want tbx upgrade refusal", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty output after preflight refusal", stdout.String())
	}
	request := <-requests
	if request.Op != "daemon.info" {
		t.Fatalf("operation = %q, want daemon.info", request.Op)
	}
	<-serverDone
}

func TestCreateClusterWithoutCNIUsesLegacyProtocolAndOutput(t *testing.T) {
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

	requests := make(chan daemon.Request, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		var request daemon.Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			return
		}
		requests <- request
		_ = json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: json.RawMessage(`{"name":"demo","controlPlanes":1,"workers":2,"nodeDefaults":{"memoryMiB":2048,"cpus":2,"diskGiB":20}}`)})
	}()

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.createCluster([]string{"demo", "--schematic=test-schematic"}); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	var args map[string]json.RawMessage
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cni", "csi", "lb", "bgp", "hubble"} {
		if _, found := args[key]; found {
			t.Fatalf("legacy create request unexpectedly includes %q: %s", key, request.Args)
		}
		if strings.Contains(stdout.String(), key+":") {
			t.Fatalf("legacy create output unexpectedly includes %q:\n%s", key, stdout.String())
		}
	}
}

func TestCreateClusterWithoutProvisioningIntentSkipsHandshake(t *testing.T) {
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

	requests := make(chan daemon.Request, 1)
	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = connection.Close() }()
		var request daemon.Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			serverDone <- err
			return
		}
		requests <- request
		serverDone <- json.NewEncoder(connection).Encode(daemon.Response{OK: true, Data: json.RawMessage(`{"name":"demo","controlPlanes":1,"workers":2,"nodeDefaults":{"memoryMiB":2048,"cpus":2,"diskGiB":20}}`)})
	}()

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.createCluster([]string{"demo", "--schematic=test-schematic"}); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	if request.Op != "cluster.create" {
		t.Fatalf("operation = %q, want cluster.create without handshake", request.Op)
	}
	if err := <-serverDone; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}
