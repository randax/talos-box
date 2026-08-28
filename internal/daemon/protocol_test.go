package daemon

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/version"
)

func TestProtocolRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
		into  any
	}{
		{
			name:  "request",
			value: Request{Op: "cluster.start", Args: json.RawMessage(`{"name":"demo"}`)},
			into:  &Request{},
		},
		{
			name:  "success response",
			value: Response{OK: true, Data: json.RawMessage(`{"pong":true}`)},
			into:  &Response{},
		},
		{
			name:  "error response",
			value: Response{OK: false, Error: "not found"},
			into:  &Response{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var wire bytes.Buffer
			if err := json.NewEncoder(&wire).Encode(test.value); err != nil {
				t.Fatal(err)
			}
			if wire.Len() == 0 || wire.Bytes()[wire.Len()-1] != '\n' {
				t.Fatalf("encoded message is not newline-delimited: %q", wire.Bytes())
			}
			if err := json.NewDecoder(&wire).Decode(test.into); err != nil {
				t.Fatal(err)
			}
			got := reflect.ValueOf(test.into).Elem().Interface()
			if !reflect.DeepEqual(got, test.value) {
				t.Fatalf("round trip = %#v, want %#v", got, test.value)
			}
		})
	}
}

func TestNodeStatusServiceFieldsRoundTripJSON(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 28, 9, 38, 0, 0, time.UTC)
	want := NodeStatus{
		Name:            "demo-worker-1",
		Services:        []NodeService{{Name: "kubelet", State: "Preparing", Health: ServiceHealthStarting, Since: &since, Message: "pulling", Restarts: 2}},
		StalledServices: []StalledService{{Service: "kubelet", State: "Preparing", Since: since}},
		ServiceProbe:    &ServiceProbe{Status: ServiceProbeSucceeded, Source: "/tmp/talosconfig"},
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"services"`, `"stalledServices"`, `"serviceProbe"`, `"since":"2026-08-28T09:38:00Z"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("node JSON missing %s: %s", field, encoded)
		}
	}
	var got NodeStatus
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("node round trip = %+v, want %+v", got, want)
	}
}

func TestProtocolVersionIncludesTalosServiceState(t *testing.T) {
	t.Parallel()
	if ProtocolVersion != 17 {
		t.Fatalf("ProtocolVersion = %d, want 17 for runtime identity, additive service fields, and cache-warm fields", ProtocolVersion)
	}
}

func TestDaemonSuccessEnrichesRuntimeIdentity(t *testing.T) {
	t.Parallel()

	response := success(Info{ProtocolVersion: ProtocolVersion, BalloonReserveMiB: 512})
	if !response.OK {
		t.Fatalf("success(info) = %+v, want OK", response)
	}
	var info Info
	if err := json.Unmarshal(response.Data, &info); err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol version = %d, want %d", info.ProtocolVersion, ProtocolVersion)
	}
	if info.Version != version.Version {
		t.Fatalf("version = %q, want %q", info.Version, version.Version)
	}
	if info.Executable == "" {
		t.Fatal("daemon info omitted Executable")
	}
	if !strings.HasSuffix(info.Executable, ".test") && !strings.Contains(info.Executable, "go-build") {
		t.Fatalf("executable = %q, want the running test binary path", info.Executable)
	}
	if info.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d", info.PID, os.Getpid())
	}
}

func TestServeConnectionRoundTrip(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	done := make(chan struct{})
	service := &Server{}
	go func() {
		service.serveConnection(server)
		close(done)
	}()

	request := Request{Op: "daemon.ping", Args: json.RawMessage(`{}`)}
	if err := json.NewEncoder(client).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || string(response.Data) != `{"pong":true}` {
		t.Fatalf("response = %#v", response)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestServeConnectionReportsProtocolVersion(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	done := make(chan struct{})
	service := &Server{}
	go func() {
		service.serveConnection(server)
		close(done)
	}()

	request := Request{Op: "daemon.info", Args: json.RawMessage(`{}`)}
	if err := json.NewEncoder(client).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("response = %#v, want success", response)
	}
	var info Info
	if err := json.Unmarshal(response.Data, &info); err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol version = %d, want %d", info.ProtocolVersion, ProtocolVersion)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestDaemonInfoIncludesRuntimeIdentity(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	done := make(chan struct{})
	service := &Server{}
	go func() {
		service.serveConnection(server)
		close(done)
	}()

	request := Request{Op: "daemon.info", Args: json.RawMessage(`{}`)}
	if err := json.NewEncoder(client).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("response = %#v, want success", response)
	}
	var info Info
	if err := json.Unmarshal(response.Data, &info); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol version = %d, want %d", info.ProtocolVersion, ProtocolVersion)
	}
	if info.Version != version.Version {
		t.Fatalf("version = %q, want %q", info.Version, version.Version)
	}
	if info.Executable != executable {
		t.Fatalf("executable = %q, want %q", info.Executable, executable)
	}
	if info.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d", info.PID, os.Getpid())
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
}
