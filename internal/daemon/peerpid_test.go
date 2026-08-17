package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPeerPIDReportsTheServingProcess(t *testing.T) {
	// a temp dir named after the test overruns the unix socket path limit
	directory, err := os.MkdirTemp("/tmp", "tbx-peerpid-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "peer.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server := <-accepted
	defer func() { _ = server.Close() }()

	pid, err := PeerPID(client)
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Fatalf("PeerPID = %d, want %d", pid, os.Getpid())
	}
}

func TestPeerPIDRejectsNonUnixConnections(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()

	if _, err := PeerPID(connection); err == nil {
		t.Fatal("PeerPID on a tcp connection must fail")
	}
}
