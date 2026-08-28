package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
)

func TestConnectRejectsProtocolVersionMismatch(t *testing.T) {
	socketPath := shortSocketPath(t, "helper-mismatch")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()

		var request Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			return
		}
		if request.Op != helperInfoOp {
			t.Errorf("handshake op = %q, want %q", request.Op, helperInfoOp)
			return
		}
		if err := sendResponse(connection.(*net.UnixConn), success(Info{ProtocolVersion: ProtocolVersion + 1}), -1); err != nil {
			t.Errorf("send response: %v", err)
		}
	}()

	t.Setenv(helperSocketEnv, socketPath)
	client, err := Connect()
	if client != nil {
		_ = client.Close()
		t.Fatal("Connect() succeeded")
	}
	if err == nil || !strings.Contains(err.Error(), hostAdviceCommand()) {
		t.Fatalf("Connect() error = %v, want reinstall guidance", err)
	}
	<-done
}

func TestProbeReturnsMismatchedHelperIdentity(t *testing.T) {
	socketPath := shortSocketPath(t, "helper-probe-mismatch")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()

		var request Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			t.Errorf("decode probe request: %v", err)
			return
		}
		if request.Op != helperInfoOp {
			t.Errorf("probe op = %q, want %q", request.Op, helperInfoOp)
			return
		}
		var args struct {
			ProtocolVersion int `json:"protocolVersion"`
		}
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Errorf("decode probe args: %v", err)
			return
		}
		if args.ProtocolVersion != ProtocolVersion {
			t.Errorf("probe protocol = %d, want %d", args.ProtocolVersion, ProtocolVersion)
		}
		info := Info{
			ProtocolVersion: ProtocolVersion + 1,
			Version:         "9.9.9",
			Executable:      "/opt/other/tbx-helper",
			PID:             4242,
		}
		if err := sendResponse(connection.(*net.UnixConn), success(info), -1); err != nil {
			t.Errorf("send probe response: %v", err)
		}
	}()

	t.Setenv(helperSocketEnv, socketPath)
	info, err := Probe()
	if err != nil {
		t.Fatalf("Probe() = %v", err)
	}
	if info.ProtocolVersion != ProtocolVersion+1 || info.Version != "9.9.9" || info.Executable != "/opt/other/tbx-helper" || info.PID != 4242 {
		t.Fatalf("Probe() = %+v, want mismatched helper identity", info)
	}
	<-done
}

func TestProbeReturnsTypedProtocolMismatchWhenOldHelperRejectsHandshake(t *testing.T) {
	socketPath := shortSocketPath(t, "helper-probe-rejected-mismatch")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()

		var request Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			t.Errorf("decode probe request: %v", err)
			return
		}
		if request.Op != helperInfoOp {
			t.Errorf("probe op = %q, want %q", request.Op, helperInfoOp)
			return
		}
		if err := sendResponse(connection.(*net.UnixConn), failure(protocolMismatchError(ProtocolVersion, ProtocolVersion-1)), -1); err != nil {
			t.Errorf("send mismatch response: %v", err)
		}
	}()

	t.Setenv(helperSocketEnv, socketPath)
	info, err := Probe()
	if info.ProtocolVersion != 0 || info.Version != "" || info.Executable != "" || info.PID != 0 {
		t.Fatalf("Probe() info = %+v, want zero identity on rejected handshake", info)
	}
	var mismatch *ProtocolMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Probe() error = %v, want typed protocol mismatch", err)
	}
	if mismatch.ClientVersion != ProtocolVersion || mismatch.HelperVersion != ProtocolVersion-1 {
		t.Fatalf("Probe() mismatch = %+v, want client=%d helper=%d", mismatch, ProtocolVersion, ProtocolVersion-1)
	}
	if !strings.Contains(err.Error(), hostAdviceCommand()) {
		t.Fatalf("Probe() error = %q, want reinstall guidance", err.Error())
	}
	<-done
}

func TestConnectStillRejectsMismatchedHelperAfterProbe(t *testing.T) {
	socketPath := shortSocketPath(t, "helper-probe-then-connect")
	t.Setenv(helperSocketEnv, socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	var connections int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for connections < 2 {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections++
			var request Request
			if err := json.NewDecoder(connection).Decode(&request); err != nil {
				t.Errorf("decode handshake %d: %v", connections, err)
				_ = connection.Close()
				return
			}
			if request.Op != helperInfoOp {
				t.Errorf("handshake %d op = %q, want %q", connections, request.Op, helperInfoOp)
				_ = connection.Close()
				return
			}
			if err := sendResponse(connection.(*net.UnixConn), success(Info{
				ProtocolVersion: ProtocolVersion + 1,
				Version:         "9.9.9",
				Executable:      "/opt/other/tbx-helper",
				PID:             4242,
			}), -1); err != nil {
				t.Errorf("send handshake %d: %v", connections, err)
			}
			_ = connection.Close()
		}
	}()

	info, err := Probe()
	if err != nil {
		t.Fatalf("Probe() = %v", err)
	}
	if info.Executable != "/opt/other/tbx-helper" {
		t.Fatalf("Probe() executable = %q, want /opt/other/tbx-helper", info.Executable)
	}
	client, err := Connect()
	if client != nil {
		_ = client.Close()
		t.Fatal("Connect() succeeded after mismatched Probe()")
	}
	if err == nil || !strings.Contains(err.Error(), "(client 5, helper 6)") {
		t.Fatalf("Connect() error = %v, want mismatch naming both protocol versions", err)
	}
	<-done
}

func TestClientReconnectsAfterHelperRestart(t *testing.T) {
	socketPath := shortSocketPath(t, "helper-restart")
	t.Setenv(helperSocketEnv, socketPath)

	firstListener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = firstListener.Close() }()

	handshakeDone := make(chan *net.UnixConn, 1)
	firstServeDone := make(chan struct{})
	go func() {
		defer close(firstServeDone)
		connection, err := firstListener.Accept()
		if err != nil {
			return
		}
		unixConnection := connection.(*net.UnixConn)

		var handshake Request
		if err := json.NewDecoder(unixConnection).Decode(&handshake); err != nil {
			t.Errorf("decode first handshake: %v", err)
			_ = unixConnection.Close()
			return
		}
		if err := sendResponse(unixConnection, success(Info{ProtocolVersion: ProtocolVersion}), -1); err != nil {
			t.Errorf("send first handshake: %v", err)
			_ = unixConnection.Close()
			return
		}
		handshakeDone <- unixConnection
	}()

	client, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	firstConnection := <-handshakeDone
	_ = firstConnection.Close()
	_ = firstListener.Close()
	<-firstServeDone
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	secondListener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = secondListener.Close() }()

	var serveErrs []error
	var serveErrMu sync.Mutex
	serveErr := func(err error) {
		if err == nil {
			return
		}
		serveErrMu.Lock()
		defer serveErrMu.Unlock()
		serveErrs = append(serveErrs, err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := secondListener.Accept()
		if err != nil {
			serveErr(err)
			return
		}
		defer func() { _ = connection.Close() }()
		decoder := json.NewDecoder(connection)

		var handshake Request
		if err := decoder.Decode(&handshake); err != nil {
			serveErr(err)
			return
		}
		if handshake.Op != helperInfoOp {
			serveErr(errors.New("unexpected handshake op"))
			return
		}
		if err := sendResponse(connection.(*net.UnixConn), success(Info{ProtocolVersion: ProtocolVersion}), -1); err != nil {
			serveErr(err)
			return
		}

		var ping Request
		if err := decoder.Decode(&ping); err != nil {
			serveErr(err)
			return
		}
		if ping.Op != "ping" {
			serveErr(errors.New("unexpected request op"))
			return
		}
		if err := sendResponse(connection.(*net.UnixConn), success(map[string]bool{"pong": true}), -1); err != nil {
			serveErr(err)
		}
	}()

	if err := client.Ping(); err != nil {
		t.Fatalf("Ping() after helper restart = %v", err)
	}
	<-done
	if len(serveErrs) != 0 {
		t.Fatalf("serve errors = %v", serveErrs)
	}
}

func TestClientDoesNotRetryUnsafeOperationAfterHelperRestart(t *testing.T) {
	socketPath := shortSocketPath(t, "helper-no-retry")
	t.Setenv(helperSocketEnv, socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	attachRead := make(chan struct{})
	retried := make(chan bool, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			retried <- false
			return
		}
		unixConnection := connection.(*net.UnixConn)
		decoder := json.NewDecoder(unixConnection)

		var handshake Request
		if err := decoder.Decode(&handshake); err != nil {
			_ = unixConnection.Close()
			retried <- false
			return
		}
		if err := sendResponse(unixConnection, success(Info{ProtocolVersion: ProtocolVersion}), -1); err != nil {
			_ = unixConnection.Close()
			retried <- false
			return
		}

		var attach Request
		if err := decoder.Decode(&attach); err != nil {
			_ = unixConnection.Close()
			retried <- false
			return
		}
		close(attachRead)
		_ = unixConnection.Close()

		_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(200 * time.Millisecond))
		retryConnection, err := listener.Accept()
		if err == nil {
			_ = retryConnection.Close()
			retried <- true
			return
		}
		retried <- false
	}()

	client, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if _, _, err := client.attach("demo", 7, "demo-cp-1"); err == nil {
		t.Fatal("attach() succeeded after helper restart")
	}
	select {
	case <-attachRead:
	case <-time.After(time.Second):
		t.Fatal("server did not receive attach request")
	}
	if <-retried {
		t.Fatal("attach() retried after helper restart")
	}
}

func shortSocketPath(t *testing.T, prefix string) string {
	t.Helper()
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.sock", prefix, os.Getpid()))
}

// A helper built before bgp.status reported routes answers with {active} alone,
// so trusting it would report "announced routes: none" for a speaker that is
// announcing. The handshake must refuse it and ask for a reinstall.
func TestConnectRejectsHelperPredatingBGPRouteReporting(t *testing.T) {
	const preRouteReportingVersion = 2
	if ProtocolVersion <= preRouteReportingVersion {
		t.Fatalf("ProtocolVersion = %d, want a version past the bgp.status route contract change", ProtocolVersion)
	}

	socketPath := shortSocketPath(t, "helper-stale-bgp")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()

		var request Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			return
		}
		if err := sendResponse(connection.(*net.UnixConn), success(Info{ProtocolVersion: preRouteReportingVersion}), -1); err != nil {
			t.Errorf("send response: %v", err)
		}
	}()

	t.Setenv(helperSocketEnv, socketPath)
	client, err := Connect()
	if client != nil {
		_ = client.Close()
		t.Fatal("Connect() succeeded against a helper that cannot report BGP routes")
	}
	if !errors.Is(err, errProtocolMismatch) {
		t.Fatalf("Connect() error = %v, want a protocol mismatch", err)
	}
	if !strings.Contains(err.Error(), hostAdviceCommand()) {
		t.Fatalf("Connect() error = %v, want reinstall guidance", err)
	}
	<-done
}

func TestSyncSendsEveryClusterReservation(t *testing.T) {
	socketPath := shortSocketPath(t, "helper-sync")
	t.Setenv(helperSocketEnv, socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	received := make(chan Request, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		unixConnection := connection.(*net.UnixConn)
		defer func() { _ = unixConnection.Close() }()
		decoder := json.NewDecoder(unixConnection)

		var handshake Request
		if err := decoder.Decode(&handshake); err != nil {
			return
		}
		if err := sendResponse(unixConnection, success(Info{ProtocolVersion: ProtocolVersion}), -1); err != nil {
			return
		}
		var request Request
		if err := decoder.Decode(&request); err != nil {
			return
		}
		received <- request
		_ = sendResponse(unixConnection, success(struct{}{}), -1)
	}()

	client, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	clusters := []cluster.Cluster{{
		Name:        "demo",
		SubnetIndex: 7,
		Nodes: []cluster.Node{
			{Name: "demo-cp-1", MAC: "52:54:00:00:07:01", IP: "172.30.7.11"},
		},
	}}
	if err := client.Sync(clusters); err != nil {
		t.Fatal(err)
	}

	var request Request
	select {
	case request = <-received:
	case <-time.After(time.Second):
		t.Fatal("helper did not receive the sync request")
	}
	if request.Op != "net.sync" {
		t.Fatalf("op = %q, want net.sync", request.Op)
	}
	var args struct {
		Clusters []SyncedCluster `json:"clusters"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	want := []SyncedCluster{{
		Name:        "demo",
		SubnetIndex: 7,
		Nodes:       []SyncedNode{{Name: "demo-cp-1", MAC: "52:54:00:00:07:01", IP: "172.30.7.11"}},
	}}
	if !reflect.DeepEqual(args.Clusters, want) {
		t.Fatalf("synced clusters = %+v, want %+v", args.Clusters, want)
	}
}

func TestSyncIsRetriedAfterAHelperRestart(t *testing.T) {
	t.Parallel()

	if !safeRetryOperation("net.sync") {
		t.Fatal("net.sync is not retried after a helper restart, so the first push after one is lost")
	}
}
