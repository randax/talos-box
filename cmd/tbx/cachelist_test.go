package main

import (
	"bytes"
	"encoding/json"
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
