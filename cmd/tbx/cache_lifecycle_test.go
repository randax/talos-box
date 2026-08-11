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

func TestRunCachePruneDefaultRequestsImageScope(t *testing.T) {
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

	done := make(chan struct{})
	go serveSingleDaemonRequest(t, listener, func(request daemon.Request) daemon.Response {
		if request.Op != "cache.prune" {
			t.Fatalf("request op = %q, want cache.prune", request.Op)
		}
		var args daemon.CachePruneArgs
		if err := json.Unmarshal(request.Args, &args); err != nil {
			t.Fatal(err)
		}
		if args.Scope != daemon.CachePruneScopeImages {
			t.Fatalf("scope = %q, want %q", args.Scope, daemon.CachePruneScopeImages)
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CachePruneResult{
			Scope:      daemon.CachePruneScopeImages,
			ImageCount: 2,
			ImageBytes: 123,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.run([]string{"cache", "prune"}); err != nil {
		t.Fatal(err)
	}
	<-done

	if got := stdout.String(); got != "pruned disk cache: 2 image(s), 123 bytes; mirror cache untouched\n" {
		t.Fatalf("stdout = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunCachePruneRejectsConflictingFlagsBeforeRPC(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.run([]string{"cache", "prune", "--mirror", "--all"})
	if err == nil {
		t.Fatal("cache prune accepted conflicting flags")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunCacheListPrintsDiskAndMirrorSections(t *testing.T) {
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

	done := make(chan struct{})
	go serveSingleDaemonRequest(t, listener, func(request daemon.Request) daemon.Response {
		if request.Op != "cache.list" {
			t.Fatalf("request op = %q, want cache.list", request.Op)
		}
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheListResult{
			Images: []daemon.CacheImageEntry{{
				Schematic:    "test-schematic",
				Version:      "v1.2.3",
				Architecture: "amd64",
				Size:         10,
			}},
			Mirror: []daemon.MirrorCacheEntry{{
				Upstream:      "docker.io",
				BlobCount:     2,
				BlobBytes:     20,
				ManifestCount: 1,
				ManifestBytes: 7,
			}},
			MirrorTotal: daemon.MirrorCacheTotals{
				BlobCount:     2,
				BlobBytes:     20,
				ManifestCount: 1,
				ManifestBytes: 7,
			},
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.run([]string{"cache", "list"}); err != nil {
		t.Fatal(err)
	}
	<-done

	want := "" +
		"Talos disk images:\n" +
		"- test-schematic v1.2.3 amd64 10 bytes\n" +
		"Mirror cache:\n" +
		"- docker.io: 2 blob(s) 20 bytes, 1 manifest(s) 7 bytes\n" +
		"Mirror total: 2 blob(s) 20 bytes, 1 manifest(s) 7 bytes\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}
