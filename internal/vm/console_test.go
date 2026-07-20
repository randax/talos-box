package vm

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startProxy(t *testing.T) (path string, guestRead, guestWrite *os.File, cleanup func()) {
	t.Helper()
	// t.TempDir() exceeds macOS's 104-byte unix socket path limit
	dir, err := os.MkdirTemp("/tmp", "tbxc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = filepath.Join(dir, "node.console.sock")
	proxy, gr, gw, err := newConsoleProxy(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, gr, gw, func() { proxy.close(); _ = gr.Close(); _ = gw.Close() }
}

func dial(t *testing.T, path string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func readWithDeadline(t *testing.T, conn net.Conn, n int) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, n)
	read, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	return string(buf[:read])
}

func TestConsoleRelaysGuestOutput(t *testing.T) {
	path, _, guestWrite, cleanup := startProxy(t)
	defer cleanup()
	client := dial(t, path)
	defer func() { _ = client.Close() }()

	if _, err := guestWrite.Write([]byte("[talos] booting\n")); err != nil {
		t.Fatal(err)
	}
	if got := readWithDeadline(t, client, 64); !strings.Contains(got, "[talos] booting") {
		t.Errorf("client read %q, want guest output", got)
	}
}

func TestConsoleRelaysClientInput(t *testing.T) {
	path, guestRead, _, cleanup := startProxy(t)
	defer cleanup()
	client := dial(t, path)
	defer func() { _ = client.Close() }()

	if _, err := client.Write([]byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	done := make(chan string, 1)
	go func() {
		n, _ := guestRead.Read(buf)
		done <- string(buf[:n])
	}()
	select {
	case got := <-done:
		if got != "ls\n" {
			t.Errorf("guest read %q, want %q", got, "ls\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guest never received client input")
	}
}

func TestConsoleSingleClientGuard(t *testing.T) {
	path, _, _, cleanup := startProxy(t)
	defer cleanup()

	first := dial(t, path)
	defer func() { _ = first.Close() }()
	// give the accept loop a moment to register the first client
	time.Sleep(50 * time.Millisecond)

	second := dial(t, path)
	got := readWithDeadline(t, second, 128)
	if !strings.Contains(got, "busy") {
		t.Errorf("second client read %q, want a busy notice", got)
	}
	// and the connection must then close
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Error("second client connection stayed open after busy notice")
	}

	// once the first detaches, a new client attaches fine
	_ = first.Close()
	time.Sleep(50 * time.Millisecond)
	third := dial(t, path)
	defer func() { _ = third.Close() }()
	time.Sleep(50 * time.Millisecond)
	if _, err := third.Write([]byte("x")); err != nil {
		t.Errorf("third client should be attached, write failed: %v", err)
	}
}
