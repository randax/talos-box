package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// TestFlaglessCachePullSendsEveryFileCombination pins the file-aware form:
// pull reads talosbox.yaml the way up does, so the daemon receives each
// cluster's pin with inheritance already applied.
func TestFlaglessCachePullSendsEveryFileCombination(t *testing.T) {
	home, requests := startUpTestDaemon(t,
		daemon.Response{OK: true, Data: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%d}`, daemon.ProtocolVersion))},
		daemon.Response{OK: true, Data: json.RawMessage(`{"schematic":"aaa","version":"v1.13.6","architecture":"arm64","path":"/cache/aaa/disk.raw","images":[
			{"schematic":"aaa","version":"v1.13.6","architecture":"arm64","path":"/cache/aaa/disk.raw"},
			{"schematic":"bbb","version":"v1.14.0","architecture":"arm64","path":"/cache/bbb/disk.raw"}]}`)},
	)
	path := filepath.Join(home, "talosbox.yaml")
	contents := `version: 1
talos:
  version: v1.13.6
clusters:
  - name: stable
  - name: shares-the-pin
  - name: canary
    talos:
      version: v1.14.0
      schematic: brought
      extensions: [gvisor]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	if err := command.runCache([]string{"pull", "-f", path}); err != nil {
		t.Fatal(err)
	}
	// Extensions are a protocol-5 field: an older daemon would drop them
	// and pull the wrong image, so the handshake comes first.
	if request := <-requests; request.Op != "daemon.info" {
		t.Fatalf("first operation = %q, want daemon.info", request.Op)
	}
	request := <-requests
	if request.Op != "cache.pull" {
		t.Fatalf("second operation = %q, want cache.pull", request.Op)
	}
	var args daemon.CachePullArgs
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	want := []daemon.CachePullCombination{
		{Version: "v1.13.6"},
		{Schematic: "brought", Version: "v1.14.0", Extensions: []string{"gvisor"}},
	}
	if !reflect.DeepEqual(args.Combinations, want) {
		t.Fatalf("combinations = %+v, want %+v", args.Combinations, want)
	}
	for _, line := range []string{
		"cached Talos v1.13.6 arm64 schematic aaa at /cache/aaa/disk.raw",
		"cached Talos v1.14.0 arm64 schematic bbb at /cache/bbb/disk.raw",
	} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("pull output missing %q:\n%s", line, stdout.String())
		}
	}
}

// TestCachePullFlagsSendOneAdHocCombination keeps the flag form single: the
// scalar fields stay populated so an older daemon still understands it.
func TestCachePullFlagsSendOneAdHocCombination(t *testing.T) {
	response := json.RawMessage(`{"schematic":"user-supplied-schematic","version":"v1.14.0","architecture":"arm64","path":"/cache/disk.raw"}`)
	request := runWithDaemonResponse(t, response, func(command cli) error {
		return command.runCache([]string{"pull", "--schematic=user-supplied-schematic", "--talos-version=v1.14.0"})
	})
	var args daemon.CachePullArgs
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	want := []daemon.CachePullCombination{{Schematic: "user-supplied-schematic", Version: "v1.14.0"}}
	if !reflect.DeepEqual(args.Combinations, want) {
		t.Fatalf("combinations = %+v, want %+v", args.Combinations, want)
	}
	if args.Schematic != "user-supplied-schematic" || args.Version != "v1.14.0" {
		t.Fatalf("scalar fields = (%q, %q), want the ad-hoc combination", args.Schematic, args.Version)
	}
}

// TestFlaglessCachePullWithoutAConfigFileKeepsTheDefaultCombination guards the
// long-standing behaviour of a bare `tbx cache pull` outside a project.
func TestFlaglessCachePullWithoutAConfigFileKeepsTheDefaultCombination(t *testing.T) {
	response := json.RawMessage(`{"schematic":"aaa","version":"v1.13.6","architecture":"arm64","path":"/cache/disk.raw"}`)
	t.Chdir(t.TempDir())

	request := runWithDaemonResponse(t, response, func(command cli) error {
		return command.runCache([]string{"pull"})
	})
	var args daemon.CachePullArgs
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	want := []daemon.CachePullCombination{{Version: daemon.DefaultTalosVersion}}
	if !reflect.DeepEqual(args.Combinations, want) {
		t.Fatalf("combinations = %+v, want %+v", args.Combinations, want)
	}
}

func TestCachePullRejectsUnknownExtensionBeforeCallingTheDaemon(t *testing.T) {
	err := (cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}}).runCache([]string{"pull", "--extensions=gvisr"})
	if err == nil || !strings.Contains(err.Error(), `did you mean "gvisor"`) {
		t.Fatalf("cache pull error = %v, want an unknown-extension refusal", err)
	}
}

func TestCachePullRejectsBelowFloorVersionBeforeCallingTheDaemon(t *testing.T) {
	err := (cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}}).runCache([]string{"pull", "--talos-version=v1.11.0"})
	if err == nil || !strings.Contains(err.Error(), "v1.11.0") {
		t.Fatalf("cache pull error = %v, want a version-floor refusal", err)
	}
}
