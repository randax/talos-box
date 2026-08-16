package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestUpRefusesUnsupportedConfigVersionBeforeDialingDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "talosbox.yaml")
	yaml := "version: 1\ntalos:\n  version: v1.11.9\nclusters:\n  - name: demo\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	err := command.runUp([]string{"-f", path})
	if err == nil || !strings.Contains(err.Error(), "v1.11.9") || !strings.Contains(err.Error(), daemon.MinTalosVersion) {
		t.Fatalf("runUp() error = %v, want refusal naming v1.11.9 and %s", err, daemon.MinTalosVersion)
	}
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
