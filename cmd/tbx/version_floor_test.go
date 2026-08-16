package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

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
