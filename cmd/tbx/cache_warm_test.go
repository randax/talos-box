package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestRunCacheWarmReadsFilesAndStdinThenPrintsSummary(t *testing.T) {
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

	first := filepath.Join(t.TempDir(), "one.txt")
	second := filepath.Join(t.TempDir(), "two.txt")
	if err := os.WriteFile(first, []byte("\n# ignored\ndocker.io/library/pause:3.10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("ghcr.io/example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go serveSingleDaemonRequest(t, listener, func(request daemon.Request) daemon.Response {
		if request.Op != "cache.warm" {
			t.Fatalf("request op = %q, want cache.warm", request.Op)
		}
		var args daemon.CacheWarmArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		wantRefs := []string{
			"docker.io/library/pause:3.10",
			"public.ecr.aws/eks-distro/kubernetes/pause:3.10",
			"ghcr.io/example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		}
		if len(args.Refs) != len(wantRefs) {
			t.Fatalf("refs len = %d, want %d (%v)", len(args.Refs), len(wantRefs), args.Refs)
		}
		for i := range wantRefs {
			if args.Refs[i] != wantRefs[i] {
				t.Fatalf("ref[%d] = %q, want %q", i, args.Refs[i], wantRefs[i])
			}
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheWarmResult{
			Entries: []daemon.CacheWarmEntry{
				{Ref: wantRefs[0], Status: daemon.CacheWarmStatusWarmed},
				{Ref: wantRefs[1], Status: daemon.CacheWarmStatusAlreadyComplete},
				{Ref: wantRefs[2], Status: daemon.CacheWarmStatusWarmed},
			},
			Warmed:          2,
			AlreadyComplete: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{
		out: &stdout,
		err: &stderr,
		in:  bytes.NewBufferString("\n# ignored\npublic.ecr.aws/eks-distro/kubernetes/pause:3.10\n"),
	}
	if err := command.run([]string{"cache", "warm", first, "-", second}); err != nil {
		t.Fatal(err)
	}
	<-done

	wantStdout := "" +
		"\u2713 docker.io/library/pause:3.10 warmed\n" +
		"\u2713 public.ecr.aws/eks-distro/kubernetes/pause:3.10 already complete\n" +
		"\u2713 ghcr.io/example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111 warmed\n" +
		"summary: 2 warmed, 1 already complete, 0 failed\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunCacheWarmRejectsInvalidRefsBeforeDaemonCall(t *testing.T) {
	listPath := filepath.Join(t.TempDir(), "images.txt")
	if err := os.WriteFile(listPath, []byte(strings.Join([]string{
		"# workshop images",
		"docker.io/library/nginx:1.27.0",
		"docker.io/library/nginx:latest",
		"docker.io/library/nginx@sha256:not-hex",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBufferString("")}
	err := command.run([]string{"cache", "warm", listPath})
	if err == nil {
		t.Fatal("run(cache warm) succeeded, want parse error")
	}
	if !strings.Contains(err.Error(), filepath.Base(listPath)+":3") || !strings.Contains(err.Error(), ":latest") {
		t.Fatalf("error = %q, want latest-tag source line rejection", err)
	}
	if !strings.Contains(err.Error(), filepath.Base(listPath)+":4") || !strings.Contains(err.Error(), "must use a sha256 or sha512 digest") {
		t.Fatalf("error = %q, want digest source line rejection", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunCacheWarmReturnsErrorWhenAnyRefFails(t *testing.T) {
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
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheWarmResult{
			Entries: []daemon.CacheWarmEntry{
				{Ref: ref, Status: daemon.CacheWarmStatusFailed, Reason: "upstream: timeout"},
			},
			Failed: 1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	err = command.run([]string{"cache", "warm", listPath})
	<-done
	if err == nil || err.Error() != "cache warm failed for 1 ref(s)" {
		t.Fatalf("err = %v, want cache warm failed for 1 ref(s)", err)
	}
	wantStdout := "" +
		"\u2717 docker.io/library/pause:3.10 upstream: timeout\n" +
		"summary: 0 warmed, 0 already complete, 1 failed\n"
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("stdout = %q, want %q", got, wantStdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}
