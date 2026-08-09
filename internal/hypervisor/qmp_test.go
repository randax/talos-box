package hypervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type qmpTestRequest struct {
	Execute   string          `json:"execute"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	ID        uint64          `json:"id"`
}

func TestNewQMPClientHandshakeAndExecute(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	serverDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(serverConn)
		if err := writeJSONLine(serverConn, map[string]any{
			"QMP": map[string]any{
				"version": map[string]any{
					"qemu": map[string]any{
						"major": 8,
						"minor": 2,
						"micro": 0,
					},
					"package": "",
				},
				"capabilities": []string{},
			},
		}); err != nil {
			serverDone <- err
			return
		}

		req, err := readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if req.Execute != "qmp_capabilities" {
			serverDone <- errors.New("unexpected handshake command")
			return
		}
		if req.ID != 1 {
			serverDone <- errors.New("unexpected handshake id")
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{"return": map[string]any{}, "id": 1}); err != nil {
			serverDone <- err
			return
		}

		req, err = readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if req.Execute != "query-status" {
			serverDone <- errors.New("unexpected command")
			return
		}
		if req.ID != 2 {
			serverDone <- errors.New("unexpected command id")
			return
		}
		var args map[string]bool
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			serverDone <- err
			return
		}
		if !args["verbose"] {
			serverDone <- errors.New("missing command arguments")
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{
			"event": "RESET",
			"data":  map[string]any{"guest": false},
		}); err != nil {
			serverDone <- err
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{
			"return": map[string]any{"status": "running"},
			"id":     2,
		}); err != nil {
			serverDone <- err
			return
		}

		serverDone <- nil
	}()

	client, err := newQMPClient(context.Background(), clientConn)
	if err != nil {
		t.Fatalf("newQMPClient() error = %v", err)
	}
	defer func() {
		if err := client.close(); err != nil {
			t.Fatalf("close() error = %v", err)
		}
	}()

	var result struct {
		Status string `json:"status"`
	}
	if err := client.execute(context.Background(), "query-status", map[string]bool{"verbose": true}, &result); err != nil {
		t.Fatalf("execute() error = %v", err)
	}
	if result.Status != "running" {
		t.Fatalf("result.Status = %q, want %q", result.Status, "running")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server script error = %v", err)
	}
}

func TestQMPClientSerializesConcurrentExecute(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	firstRequestRead := make(chan uint64, 1)
	serverDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(serverConn)
		if err := writeJSONLine(serverConn, map[string]any{
			"QMP": map[string]any{"capabilities": []string{}},
		}); err != nil {
			serverDone <- err
			return
		}
		req, err := readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if req.Execute != "qmp_capabilities" || req.ID != 1 {
			serverDone <- errors.New("unexpected handshake request")
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{"return": map[string]any{}, "id": 1}); err != nil {
			serverDone <- err
			return
		}

		req, err = readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		firstRequestRead <- req.ID

		_ = serverConn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		if _, err := reader.ReadBytes('\n'); err == nil {
			serverDone <- errors.New("second request arrived before first completed")
			return
		} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			serverDone <- err
			return
		}
		_ = serverConn.SetReadDeadline(time.Time{})

		if err := writeJSONLine(serverConn, map[string]any{"return": map[string]any{}, "id": req.ID}); err != nil {
			serverDone <- err
			return
		}

		req, err = readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if req.ID != 3 {
			serverDone <- errors.New("unexpected second request id")
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{"return": map[string]any{}, "id": req.ID}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	client, err := newQMPClient(context.Background(), clientConn)
	if err != nil {
		t.Fatalf("newQMPClient() error = %v", err)
	}
	defer func() {
		if err := client.close(); err != nil {
			t.Fatalf("close() error = %v", err)
		}
	}()

	firstErr := make(chan error, 1)
	go func() {
		firstErr <- client.execute(context.Background(), "first", nil, nil)
	}()

	select {
	case <-firstRequestRead:
	case <-time.After(2 * time.Second):
		t.Fatal("server never observed first request")
	}

	secondErr := make(chan error, 1)
	go func() {
		secondErr <- client.execute(context.Background(), "second", nil, nil)
	}()

	if err := <-firstErr; err != nil {
		t.Fatalf("first execute() error = %v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second execute() error = %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server script error = %v", err)
	}
}

func TestQMPClientReturnsTypedDeviceNotActiveError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	serverDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(serverConn)
		if err := writeJSONLine(serverConn, map[string]any{"QMP": map[string]any{"capabilities": []string{}}}); err != nil {
			serverDone <- err
			return
		}
		req, err := readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{"return": map[string]any{}, "id": req.ID}); err != nil {
			serverDone <- err
			return
		}
		req, err = readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{
			"error": map[string]any{
				"class": "DeviceNotActive",
				"desc":  "device is not active",
			},
			"id": req.ID,
		}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	client, err := newQMPClient(context.Background(), clientConn)
	if err != nil {
		t.Fatalf("newQMPClient() error = %v", err)
	}

	err = client.execute(context.Background(), "device_del", nil, nil)
	if err == nil {
		t.Fatal("execute() error = nil, want typed QMP error")
	}
	if !errors.Is(err, ErrDeviceNotActive) {
		t.Fatalf("errors.Is(%v, ErrDeviceNotActive) = false", err)
	}
	var qerr *qmpError
	if !errors.As(err, &qerr) {
		t.Fatalf("errors.As(%v, *qmpError) = false", err)
	}
	if qerr.Class != "DeviceNotActive" || qerr.Desc != "device is not active" {
		t.Fatalf("qmpError = %#v, want class/desc preserved", qerr)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server script error = %v", err)
	}
}

func TestQMPClientIgnoresStaleResponseAfterDeadline(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	releaseSecondPhase := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(serverConn)
		if err := writeJSONLine(serverConn, map[string]any{"QMP": map[string]any{"capabilities": []string{}}}); err != nil {
			serverDone <- err
			return
		}
		req, err := readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{"return": map[string]any{}, "id": req.ID}); err != nil {
			serverDone <- err
			return
		}

		staleReq, err := readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		<-releaseSecondPhase

		nextReq, err := readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{"return": map[string]any{}, "id": staleReq.ID}); err != nil {
			serverDone <- err
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{
			"return": map[string]any{"enabled": true},
			"id":     nextReq.ID,
		}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	client, err := newQMPClient(context.Background(), clientConn)
	if err != nil {
		t.Fatalf("newQMPClient() error = %v", err)
	}
	defer func() {
		if err := client.close(); err != nil {
			t.Fatalf("close() error = %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = client.execute(ctx, "query-migrate", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("execute(timeout) error = %v, want context.DeadlineExceeded", err)
	}

	close(releaseSecondPhase)

	var result struct {
		Enabled bool `json:"enabled"`
	}
	if err := client.execute(context.Background(), "query-balloon", nil, &result); err != nil {
		t.Fatalf("second execute() error = %v", err)
	}
	if !result.Enabled {
		t.Fatalf("second execute result = %#v, want enabled response", result)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server script error = %v", err)
	}
}

func TestQMPClientCloseIsIdempotent(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()

	serverDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(serverConn)
		if err := writeJSONLine(serverConn, map[string]any{"QMP": map[string]any{"capabilities": []string{}}}); err != nil {
			serverDone <- err
			return
		}
		req, err := readQMPTestRequest(reader, serverConn)
		if err != nil {
			serverDone <- err
			return
		}
		if err := writeJSONLine(serverConn, map[string]any{"return": map[string]any{}, "id": req.ID}); err != nil {
			serverDone <- err
			return
		}
		_, err = reader.ReadByte()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	client, err := newQMPClient(context.Background(), clientConn)
	if err != nil {
		t.Fatalf("newQMPClient() error = %v", err)
	}

	if err := client.close(); err != nil {
		t.Fatalf("first close() error = %v", err)
	}
	if err := client.close(); err != nil {
		t.Fatalf("second close() error = %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server script error = %v", err)
	}
}

func TestQMPClientRejectsOversizedFrame(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	go func() {
		_, _ = serverConn.Write(make([]byte, maxQMPFrame+2))
		_ = serverConn.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := newQMPClient(ctx, clientConn)
	if err == nil || !strings.Contains(err.Error(), "qmp frame exceeds") {
		t.Fatalf("newQMPClient() = %v, want oversized-frame error", err)
	}
}

func writeJSONLine(conn net.Conn, value any) error {
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	return json.NewEncoder(conn).Encode(value)
}

func readQMPTestRequest(reader *bufio.Reader, conn net.Conn) (qmpTestRequest, error) {
	var req qmpTestRequest
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return req, err
	}
	return req, json.Unmarshal(line, &req)
}
