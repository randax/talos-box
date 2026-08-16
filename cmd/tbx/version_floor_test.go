package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestEmptyTalosVersionFlagMeansDaemonDefault(t *testing.T) {
	// An explicit empty value defers to the daemon default, matching the
	// daemon boundary's own convention — it must not be refused as malformed.
	response := json.RawMessage(`{"name":"demo","controlPlanes":1,"workers":2,"nodeDefaults":{"memoryMiB":2048,"cpus":2,"diskGiB":20}}`)
	runWithDaemonResponse(t, response, func(command cli) error {
		return command.createCluster([]string{"demo", "--talos-version="})
	})
}

func TestCachePullRefusesUnsupportedVersionBeforeDialingDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	err := command.runCache([]string{"pull", "--talos-version=v1.11.9"})
	if err == nil || !strings.Contains(err.Error(), "v1.11.9") || !strings.Contains(err.Error(), daemon.MinTalosVersion) {
		t.Fatalf("runCache(pull) error = %v, want refusal naming v1.11.9 and %s", err, daemon.MinTalosVersion)
	}
}

func TestCreateClusterRefusesUnsupportedVersionsBeforeDialingDaemon(t *testing.T) {
	// No daemon socket exists under this HOME: reaching the wire would fail
	// with a dial error, so a version-shaped error proves the CLI refused.
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		name    string
		version string
		wantErr string
	}{
		{"below floor names both versions", "v1.11.9", daemon.MinTalosVersion},
		{"malformed version", "latest", `"latest"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			command := cli{out: &out, err: &errOut}
			err := command.createCluster([]string{"demo", "--talos-version=" + tt.version})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("createCluster(--talos-version=%s) error = %v, want containing %q", tt.version, err, tt.wantErr)
			}
			if tt.version != "latest" && !strings.Contains(err.Error(), tt.version) {
				t.Fatalf("error %v must name the requested version %s", err, tt.version)
			}
		})
	}
}
