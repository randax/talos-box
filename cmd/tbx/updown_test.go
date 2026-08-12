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
	cfg, force, quiet, err := loadUpConfigFile([]string{"-f", path, "--force"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].Name != "demo" || !force || quiet {
		t.Fatalf("loadUpConfigFile() = clusters %+v, force %v, quiet %v; want demo, true, false", cfg.Clusters, force, quiet)
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

func TestPrintActionsNamesIncompleteProvisioningAsReconciliation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.printActions(
		[]daemon.Action{{Cluster: "demo", Kind: daemon.ActionReconcile}},
		map[daemon.ActionKind]string{daemon.ActionReconcile: "reconciled %s", daemon.ActionNone: "%s is up to date"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "reconciled demo\n" {
		t.Fatalf("stdout = %q, want reconciliation instead of up-to-date", got)
	}
}

func TestPrintActionsQuietKeepsFinalOutputButSuppressesNarration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.printActions(
		[]daemon.Action{{Cluster: "demo", Kind: daemon.ActionCreate, Narration: []string{"≈ talosctl apply-config"}}},
		map[daemon.ActionKind]string{daemon.ActionCreate: "created %s"}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "created demo\n" {
		t.Fatalf("quiet output = %q, want final action only", got)
	}
}
