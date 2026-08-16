package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// TestCreateClusterSendsExtensionsAndLeavesCompositionToTheDaemon pins the
// division of labour: with extensions requested the client must not resolve a
// schematic of its own (that would compose the extension-free default), it
// sends the request through and the daemon composes.
func TestCreateClusterSendsExtensionsAndLeavesCompositionToTheDaemon(t *testing.T) {
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
		_ = acceptAndRespond(daemon.Response{OK: true, Data: json.RawMessage(`{"name":"demo","controlPlanes":1,"workers":2,"nodeDefaults":{"memoryMiB":2048,"cpus":2,"diskGiB":20},"schematic":"composed-id","talosVersion":"v1.13.6"}`)})
	}()

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.createCluster([]string{"demo", "--extensions=gvisor, nfs-utils"}); err != nil {
		t.Fatal(err)
	}
	if infoRequest := <-requests; infoRequest.Op != "daemon.info" {
		t.Fatalf("first operation = %q, want daemon.info", infoRequest.Op)
	}
	request := <-requests
	if request.Op != "cluster.create" {
		t.Fatalf("second operation = %q, want cluster.create", request.Op)
	}
	var args struct {
		Schematic  string   `json:"schematic"`
		Extensions []string `json:"extensions"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if want := []string{"gvisor", "nfs-utils"}; !reflect.DeepEqual(args.Extensions, want) {
		t.Fatalf("create extensions = %v, want %v", args.Extensions, want)
	}
	if args.Schematic != "" {
		t.Fatalf("create schematic = %q, want it left to the daemon", args.Schematic)
	}
}

func TestCreateClusterRejectsUnknownExtensionBeforeTheDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.createCluster([]string{"demo", "--extensions=gvisr"})
	if err == nil || !strings.Contains(err.Error(), `did you mean "gvisor"`) {
		t.Fatalf("createCluster() error = %v, want an unknown-extension refusal", err)
	}
}

func TestParseExtensionList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "empty means none requested"},
		{name: "single", value: "gvisor", want: []string{"gvisor"}},
		{name: "spaces around separators", value: " gvisor , nfs-utils ", want: []string{"gvisor", "nfs-utils"}},
		{name: "trailing separator", value: "gvisor,", want: []string{"gvisor"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := parseExtensionList(test.value); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseExtensionList(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}
