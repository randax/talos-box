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

const cacheListResponse = `{"images":[{"schematic":"abc","version":"v1.13.6","architecture":"arm64","size":10,"allocatedSize":4,"status":"orphan"},` +
	`{"schematic":"def","version":"v1.13.6","architecture":"arm64","size":99302020,"status":"orphan","incomplete":true}],` +
	`"mirror":[],"mirrorTotal":{"blobCount":0,"blobBytes":0,"manifestCount":0,"manifestBytes":0},"mirrorBoundGatewayIps":null}`

func TestCacheListJSONEmitsTheDaemonResult(t *testing.T) {
	// cache list needs no protocol handshake, so cache.list is the only call.
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(cacheListResponse)},
	})

	if err := command.runCache([]string{"list", "-o", "json"}); err != nil {
		t.Fatal(err)
	}

	if op := (<-requests).Op; op != "cache.list" {
		t.Fatalf("request op = %q, want cache.list", op)
	}
	var result daemon.CacheListResult
	if err := json.Unmarshal(command.out.(*bytes.Buffer).Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, command.out.(*bytes.Buffer).String())
	}
	if len(result.Images) != 2 || result.Images[1].Schematic != "def" || !result.Images[1].Incomplete {
		t.Fatalf("images = %+v, want both entries with the incomplete one marked", result.Images)
	}
	if got := command.out.(*bytes.Buffer).String(); strings.Contains(got, "null") {
		t.Fatalf("stdout = %q, want empty slices rendered as [] not null", got)
	}
}

func TestCacheListRejectsUnknownOutputFormat(t *testing.T) {
	command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	if err := command.runCache([]string{"list", "-o", "yaml"}); err == nil {
		t.Fatal("cache list accepted an unknown output format")
	}
}

func TestCacheListTableMarksIncompleteCombinations(t *testing.T) {
	// cache list needs no protocol handshake, so cache.list is the only call.
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(cacheListResponse)},
	})

	if err := command.runCache([]string{"list"}); err != nil {
		t.Fatal(err)
	}
	<-requests

	got := command.out.(*bytes.Buffer).String()
	if !strings.Contains(got, "def v1.13.6 arm64 99302020 bytes (incomplete) orphan") {
		t.Fatalf("stdout = %q, want the incomplete orphan previewed", got)
	}
	if strings.Contains(got, "abc v1.13.6 arm64 10 bytes (4 bytes on disk) (incomplete)") {
		t.Fatalf("stdout = %q, want the ready image left unmarked", got)
	}
}

// `cache list` is aggregate-only, which cannot answer "is this ref cached";
// naming a ref answers it from the same verification `--check` performs (#406).
func TestCacheListRefAnswersCachedAndNotCached(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		entry daemon.CacheCheckEntry
		count daemon.CacheCheckResult
		want  string
	}{
		{
			name:  "cached",
			entry: daemon.CacheCheckEntry{Ref: "docker.io/library/busybox:1.37", Status: daemon.CacheCheckStatusComplete},
			count: daemon.CacheCheckResult{Complete: 1},
			want:  "docker.io/library/busybox:1.37: cached\n",
		},
		{
			name: "not cached",
			entry: daemon.CacheCheckEntry{
				Ref: "docker.io/library/busybox:1.37", Status: daemon.CacheCheckStatusFailed,
				Reason: "/v2/library/busybox/manifests/1.37 not cached",
			},
			count: daemon.CacheCheckResult{Failed: 1},
			want:  "docker.io/library/busybox:1.37: not cached (/v2/library/busybox/manifests/1.37 not cached)\n",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
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
			go serveDaemonRequests(t, listener, 1, func(_ int, request daemon.Request) daemon.Response {
				if request.Op != "cache.check" {
					t.Fatalf("request op = %q, want cache.check", request.Op)
				}
				var args daemon.CacheCheckArgs
				if err := json.Unmarshal(request.Args, &args); err != nil {
					t.Fatal(err)
				}
				if args.Deep {
					t.Fatal("cache list <ref> asked for a deep check")
				}
				if len(args.Refs) != 1 || args.Refs[0] != testCase.entry.Ref {
					t.Fatalf("refs = %v, want [%q]", args.Refs, testCase.entry.Ref)
				}
				result := testCase.count
				result.Entries = []daemon.CacheCheckEntry{testCase.entry}
				return daemon.Response{OK: true, Data: mustJSON(t, result)}
			}, done)

			var stdout, stderr bytes.Buffer
			command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
			if err := command.run([]string{"cache", "list", testCase.entry.Ref}); err != nil {
				t.Fatalf("cache list %s err = %v, want nil: a lookup reports, it does not gate", testCase.entry.Ref, err)
			}
			<-done
			if got := stdout.String(); got != testCase.want {
				t.Fatalf("stdout = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestCacheListRefUsesSameStructuredCompletenessReason(t *testing.T) {
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

	ref := "docker.io/library/busybox:1.37"
	reason := "2 of 7 linux/amd64 layers not cached: sha256:2222, sha256:3333"
	done := make(chan struct{})
	go serveDaemonRequests(t, listener, 1, func(_ int, request daemon.Request) daemon.Response {
		return daemon.Response{OK: true, Data: mustJSON(t, daemon.CacheCheckResult{
			Entries: []daemon.CacheCheckEntry{{Ref: ref, Status: daemon.CacheCheckStatusFailed, Reason: reason}},
			Failed:  1,
		})}
	}, done)

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, in: bytes.NewBuffer(nil)}
	if err := command.run([]string{"cache", "list", ref}); err != nil {
		t.Fatal(err)
	}
	<-done
	want := ref + ": not cached (" + reason + ")\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestCacheListRefRejectsAnInvalidReferenceBeforeCallingTheDaemon(t *testing.T) {
	command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: bytes.NewBuffer(nil)}
	if err := command.runCache([]string{"list", "not a ref"}); err == nil {
		t.Fatal("cache list accepted an invalid image reference")
	}
}

func TestCacheListRejectsMoreThanOneReference(t *testing.T) {
	command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: bytes.NewBuffer(nil)}
	err := command.runCache([]string{"list", "docker.io/library/busybox:1.37", "docker.io/library/busybox:1.36"})
	if err == nil || !strings.Contains(err.Error(), "usage: tbx cache list") {
		t.Fatalf("err = %v, want the cache list usage", err)
	}
}
