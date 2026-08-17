package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestPrintWarningsRendersOnePerLine(t *testing.T) {
	var stderr bytes.Buffer
	warnings := []string{
		"qa-sta-cp-1: saved state could not be restored; cold-booting instead: hypervisor saved state is incompatible",
		"qa-sta-worker-1: saved state could not be restored; cold-booting instead: hypervisor saved state is incompatible",
		"host route for 172.30.0.0/24 goes through utun9",
	}
	if err := printWarnings(&stderr, warnings, strings.Join(warnings, "; ")); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) != len(warnings) {
		t.Fatalf("stderr = %q, want %d lines", stderr.String(), len(warnings))
	}
	for i, line := range lines {
		if line != "warning: "+warnings[i] {
			t.Fatalf("line %d = %q, want %q", i, line, "warning: "+warnings[i])
		}
	}
}

func TestPrintWarningsFallsBackToTheLegacyString(t *testing.T) {
	var stderr bytes.Buffer
	if err := printWarnings(&stderr, nil, "a; b"); err != nil {
		t.Fatal(err)
	}
	if got := stderr.String(); got != "warning: a; b\n" {
		t.Fatalf("stderr = %q, want the legacy joined warning", got)
	}
}

func TestPrintActionsRendersEachWarningOnItsOwnLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.printActions(
		[]daemon.Action{{
			Cluster:  "demo",
			Kind:     daemon.ActionStart,
			Warning:  "first; second",
			Warnings: []string{"first", "second"},
		}},
		map[daemon.ActionKind]string{daemon.ActionStart: "started %s"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stderr.String(); got != "warning: first\nwarning: second\n" {
		t.Fatalf("stderr = %q, want one warning per line", got)
	}
}
