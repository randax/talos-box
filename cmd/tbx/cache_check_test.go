package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestRunCacheWarmCheckRequestsOfflineVerification(t *testing.T) {
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

	listPath := filepath.Join(t.TempDir(), "images.txt")
	ref := "docker.io/library/pause:3.10"
	if err := os.WriteFile(listPath, []byte(ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go serveSingleDaemonRequest(t, listener, func(request daemon.Request) daemon.Response {
		if request.Op != "cache.check" {
			t.Fatalf("request op = %q, want cache.check", request.Op)
		}
		var args daemon.CacheCheckArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if args.Deep {
			t.Fatal("cache.check deep = true, want false")
		}
		if len(args.Refs) != 1 || args.Refs[0] != ref {
			t.Fatalf("refs = %v, want [%q]", args.Refs, ref)
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
			Entries:  []daemon.CacheCheckEntry{{Ref: ref, Status: daemon.CacheCheckStatusComplete}},
			Complete: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	if err := command.run([]string{"cache", "warm", "--check", listPath}); err != nil {
		t.Fatal(err)
	}
	<-done

	wantStdout := "" +
		"\u2713 docker.io/library/pause:3.10 complete\n" +
		"summary: 1 complete, 0 failed\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunCacheWarmCheckDeepRequestsDeepVerification(t *testing.T) {
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

	listPath := filepath.Join(t.TempDir(), "images.txt")
	ref := "docker.io/library/pause:3.10"
	if err := os.WriteFile(listPath, []byte(ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go serveSingleDaemonRequest(t, listener, func(request daemon.Request) daemon.Response {
		var args daemon.CacheCheckArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if !args.Deep {
			t.Fatal("cache.check deep = false, want true")
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
			Entries: []daemon.CacheCheckEntry{{Ref: ref, Status: daemon.CacheCheckStatusFailed, Reason: "sha256:deadbeef blob corrupted"}},
			Failed:  1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"cache", "warm", "--check", "--deep", listPath})
	<-done
	if err == nil || err.Error() != "cache check failed for 1 ref(s)" {
		t.Fatalf("err = %v, want cache check failed for 1 ref(s)", err)
	}
	wantStdout := "" +
		"\u2717 docker.io/library/pause:3.10 sha256:deadbeef blob corrupted\n" +
		"summary: 0 complete, 1 failed\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunCacheWarmRejectsDeepWithoutCheck(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err := command.run([]string{"cache", "warm", "--deep", "images.txt"})
	if err == nil || err.Error() != "cache warm --deep requires --check" {
		t.Fatalf("err = %v, want cache warm --deep requires --check", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}
