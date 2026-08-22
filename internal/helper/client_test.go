package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
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
		if err := sendResponse(connection.(*net.UnixConn), success(Info{ProtocolVersion: protocolVersion + 1}), -1); err != nil {
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
		if err := sendResponse(unixConnection, success(Info{ProtocolVersion: protocolVersion}), -1); err != nil {
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
		if err := sendResponse(connection.(*net.UnixConn), success(Info{ProtocolVersion: protocolVersion}), -1); err != nil {
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
		if err := sendResponse(unixConnection, success(Info{ProtocolVersion: protocolVersion}), -1); err != nil {
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
	return fmt.Sprintf("/tmp/%s-%d.sock", prefix, os.Getpid())
}

// A helper built before bgp.status reported routes answers with {active} alone,
// so trusting it would report "announced routes: none" for a speaker that is
// announcing. The handshake must refuse it and ask for a reinstall.
func TestConnectRejectsHelperPredatingBGPRouteReporting(t *testing.T) {
	const preRouteReportingVersion = 2
	if protocolVersion <= preRouteReportingVersion {
		t.Fatalf("protocolVersion = %d, want a version past the bgp.status route contract change", protocolVersion)
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
