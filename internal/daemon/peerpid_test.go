package daemon

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// peerPIDHelperEnv makes this test binary re-run as the socket-serving child
// process, so PeerPID is asserted against a pid that is provably not our own.
const peerPIDHelperEnv = "TBX_PEERPID_HELPER_SOCKET"

// TestPeerPIDHelperProcess is the child: it serves the socket named by the
// environment and holds it open until the parent kills it. It is not a test of
// its own — without the environment variable it does nothing.
func TestPeerPIDHelperProcess(t *testing.T) {
	socket := os.Getenv(peerPIDHelperEnv)
	if socket == "" {
		t.Skip("helper process; not run directly")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	connection, err := listener.Accept()
	if err != nil {
		return
	}
	defer func() { _ = connection.Close() }()
	// hold the connection open; the parent inspects it and kills us
	time.Sleep(30 * time.Second)
}

func TestPeerPIDReportsTheServingProcess(t *testing.T) {
	// a temp dir named after the test overruns the unix socket path limit
	directory, err := os.MkdirTemp("/tmp", "tbx-peerpid-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "peer.sock")

	child := exec.Command(os.Args[0], "-test.run=^TestPeerPIDHelperProcess$", "-test.timeout=60s")
	child.Env = append(os.Environ(), peerPIDHelperEnv+"="+socket)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	connection := dialWhenServed(t, socket)
	defer func() { _ = connection.Close() }()

	pid, err := PeerPID(connection)
	if err != nil {
		t.Fatal(err)
	}
	if pid != child.Process.Pid {
		t.Fatalf("PeerPID = %d, want the serving child %d", pid, child.Process.Pid)
	}
	if pid == os.Getpid() {
		t.Fatal("PeerPID reported this process, not the process serving the socket")
	}
}

// dialWhenServed waits for the child to bind the socket.
func dialWhenServed(t *testing.T, socket string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		connection, err := net.Dial("unix", socket)
		if err == nil {
			return connection
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper process never served %s: %v", socket, err)
		}
		time.Sleep(20 * time.Millisecond)
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
