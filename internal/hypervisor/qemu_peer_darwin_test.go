//go:build darwin

package hypervisor

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestVerifyQMPPeerDarwinMatchesProcessAndUID(t *testing.T) {
	client, server := darwinQMPPeerConnections(t)
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyQMPPeerDarwin(client, &qemuProcess{process: process}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyQMPPeerDarwinRejectsDifferentPID(t *testing.T) {
	client, server := darwinQMPPeerConnections(t)
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	process, err := os.FindProcess(os.Getpid() + 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyQMPPeerDarwin(client, &qemuProcess{process: process}); err == nil {
		t.Fatal("verifyQMPPeerDarwin accepted a different process")
	}
}

func darwinQMPPeerConnections(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	descriptors, err := unix.Socketpair(unix.AF_LOCAL, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	clientFile := os.NewFile(uintptr(descriptors[0]), "qmp-client")
	serverFile := os.NewFile(uintptr(descriptors[1]), "qmp-server")
	client, err := net.FileConn(clientFile)
	if err != nil {
		t.Fatal(err)
	}
	server, err := net.FileConn(serverFile)
	if err != nil {
		t.Fatal(err)
	}
	_ = clientFile.Close()
	_ = serverFile.Close()
	return client, server
}
