package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
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

// A daemon doing honest work past the timeout — status probing node after
// node — keeps narrating, and every stage re-arms the silence deadline, so
// doctor waits for the real answer instead of reporting a stall.
func TestDoctorCallWaitsForNarratedSlowDaemon(t *testing.T) {
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

	previous := doctorCallTimeout
	doctorCallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { doctorCallTimeout = previous })

	var narrated atomic.Bool
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		var request daemon.Request
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		narrated.Store(request.Progress)
		encoder := json.NewEncoder(connection)
		// four stages, each inside the silence window, spanning 2x the timeout
		for index := 0; index < 4; index++ {
			time.Sleep(doctorCallTimeout / 2)
			if err := encoder.Encode(daemon.Response{Stage: fmt.Sprintf("probing node demo/n-%d", index)}); err != nil {
				t.Error(err)
				return
			}
		}
		data, _ := json.Marshal([]daemon.ClusterStatus{{Name: "demo"}})
		if err := encoder.Encode(daemon.Response{OK: true, Data: data}); err != nil {
			t.Error(err)
		}
	}()

	var statuses []daemon.ClusterStatus
	if err := (cli{}).doctorCall("status", map[string]string{"cluster": ""}, &statuses); err != nil {
		t.Fatalf("doctorCall on a narrating slow daemon: %v", err)
	}
	if !narrated.Load() {
		t.Fatal("doctor did not ask the daemon for progress narration")
	}
	if len(statuses) != 1 || statuses[0].Name != "demo" {
		t.Fatalf("statuses = %+v, want the daemon's answer", statuses)
	}
}
