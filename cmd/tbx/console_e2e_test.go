//go:build e2e

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestConsoleE2E boots a real node and attaches to its console during boot:
// kernel/machined logs must stream, and a second attach must be refused.
// Requires: vz-capable Mac, tbx-helper installed, image cache warm.
func TestConsoleE2E(t *testing.T) {
	bin := binPath(t, "tbx")
	name := "e2econ"

	t.Cleanup(func() {
		_ = exec.Command(bin, "cluster", "destroy", name, "--force").Run()
	})
	create := exec.Command(bin, "cluster", "create", name, "--workers", "0")
	if out, err := create.CombinedOutput(); err != nil {
		// create blocks until boot; run it async via Start instead
		t.Fatalf("create: %v: %s", err, out)
	}

	home, _ := os.UserHomeDir()
	sock := filepath.Join(home, ".talosbox", "clusters", name, name+"-cp-1.console.sock")
	waitFor(t, 30*time.Second, func() bool { _, err := os.Stat(sock); return err == nil })

	// first client: attach and stop the cluster's VM activity by restarting it,
	// which reboots the node and replays boot output into our attachment
	console := exec.Command(bin, "console", name, name+"-cp-1")
	var captured bytes.Buffer
	console.Stdout = &captured
	console.Stdin = nil
	if err := console.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = console.Process.Kill() }()

	restart := exec.Command(bin, "cluster", "stop", name)
	if out, err := restart.CombinedOutput(); err != nil {
		t.Fatalf("stop: %v: %s", err, out)
	}
	start := exec.Command(bin, "cluster", "start", name)
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start: %v: %s", err, out)
	}
	// reattach after restart (the old socket died with the VM)
	waitFor(t, 30*time.Second, func() bool { _, err := os.Stat(sock); return err == nil })
	console2 := exec.Command(bin, "console", name, name+"-cp-1")
	var captured2 bytes.Buffer
	console2.Stdout = &captured2
	if err := console2.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = console2.Process.Kill() }()

	waitFor(t, 90*time.Second, func() bool {
		return strings.Contains(captured2.String(), "[talos]")
	})
	if !strings.Contains(captured2.String(), "[talos]") {
		t.Fatalf("no machined output captured; got %d bytes", captured2.Len())
	}

	// busy guard: a concurrent attach must be refused with the busy notice
	busy := exec.Command(bin, "console", name, name+"-cp-1")
	out, _ := busy.Output()
	if !strings.Contains(string(out), "busy") {
		t.Errorf("second attach output %q, want busy notice", out)
	}
}

func binPath(t *testing.T, name string) string {
	t.Helper()
	wd, _ := os.Getwd()
	p := filepath.Join(wd, "..", "..", "bin", name)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("bin/%s not built (run make build)", name)
	}
	return p
}

func waitFor(t *testing.T, limit time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(time.Second)
	}
}
