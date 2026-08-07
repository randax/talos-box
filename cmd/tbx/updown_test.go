package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestLoadUpConfigFileAcceptsForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "talosbox.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nclusters:\n  - name: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, force, err := loadUpConfigFile([]string{"-f", path, "--force"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].Name != "demo" || !force {
		t.Fatalf("loadUpConfigFile() = clusters %+v, force %v; want demo, true", cfg.Clusters, force)
	}
}

func TestPrintActionsWritesWarningsToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.printActions(
		[]daemon.Action{{Cluster: "demo", Kind: daemon.ActionStart, Warning: "host pressure (forced)"}},
		map[daemon.ActionKind]string{daemon.ActionStart: "started %s"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "started demo\n" {
		t.Fatalf("stdout = %q, want start action", got)
	}
	if got := stderr.String(); !strings.Contains(got, "warning: host pressure (forced)") {
		t.Fatalf("stderr = %q, want host-pressure warning", got)
	}
}
