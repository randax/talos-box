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
// Requires: an available selected hypervisor, tbx-helper installed, image cache warm.
func TestConsoleE2E(t *testing.T) {
	backend := requireE2EHypervisor(t)
	requireNoForeignClusters(t)
	bin := binPath(t, "tbx")
	name := uniqueE2EClusterName("console")
	yaml, err := renderE2EConfig(validE2ETestConfig(name, backend.Name))
	if err != nil {
		t.Fatalf("render console e2e config: %v", err)
	}
	configPath := writeE2EConfig(t, yaml)
	logOffset := captureTBXDLogOffset(t)
	var cleanupOutput strings.Builder
	registerE2EFailureDiagnostics(t, logOffset, &cleanupOutput)
	runTBX(t, "up", "-f", configPath)
	registerE2EClusterCleanup(t, name, &cleanupOutput)

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

	runTBX(t, "cluster", "stop", name)
	runTBX(t, "cluster", "start", name)
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
