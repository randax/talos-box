package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/daemon"
)

// A daemon that accepts the socket but never answers must not hang doctor:
// every diagnostic RPC goes through doctorCall, so the bound lives there.
func TestDoctorCallGivesUpOnSilentDaemon(t *testing.T) {
	t.Setenv("HOME", shortTestHome(t))
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			defer func() { _ = connection.Close() }()
			<-t.Context().Done() // hold the request open without answering
		}
	}()

	previous := doctorCallTimeout
	doctorCallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { doctorCallTimeout = previous })

	started := time.Now()
	var status []daemon.ClusterStatus
	err = (cli{}).doctorCall("status", map[string]string{"cluster": ""}, &status)
	if !isTimeout(err) {
		t.Fatalf("doctorCall error = %v, want a timeout", err)
	}
	if isDaemonUnavailable(err) {
		t.Fatalf("a silent daemon is running, not absent: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*doctorCallTimeout {
		t.Fatalf("doctorCall took %v, want about %v", elapsed, doctorCallTimeout)
	}
}
