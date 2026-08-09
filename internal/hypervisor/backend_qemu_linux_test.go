//go:build linux

package hypervisor

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeKVMRejectsUnavailableDevice(t *testing.T) {
	err := probeKVM(filepath.Join(t.TempDir(), "missing-kvm"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("probeKVM() = %v, want ErrUnsupported", err)
	}
}

func TestProbeKVMAcceptsWritableDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeKVMWith(path, func(uintptr) (int, error) { return 12, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyQMPPeerMatchesSpawnedProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qmp.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, _ := listener.Accept()
		accepted <- connection
	}()
	client, err := (&net.Dialer{}).DialContext(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server := <-accepted
	defer func() { _ = server.Close() }()
	current, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyQMPPeer(client, &qemuProcess{process: current}); err != nil {
		t.Fatal(err)
	}
	other, err := os.FindProcess(os.Getpid() + 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyQMPPeer(client, &qemuProcess{process: other}); err == nil {
		t.Fatal("verifyQMPPeer accepted a different process")
	}
}

func TestNewQEMUConsoleProxyCreatesNestedDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "console", "node.sock")
	proxy, guest, err := newQEMUConsoleProxy(path)
	if err != nil {
		t.Fatal(err)
	}
	proxy.close()
	if err := guest.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("console directory was not created: %v", err)
	}
}
