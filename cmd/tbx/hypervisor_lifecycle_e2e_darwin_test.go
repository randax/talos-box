//go:build e2e && darwin

package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/hypervisor"
)

const hypervisorLifecycleStatusTimeout = 5 * time.Minute

func TestQEMUSuspendSurvivesDaemonRestartE2E(t *testing.T) {
	backend := requireE2EHypervisor(t)
	if backend.Name != hypervisor.NameQEMU {
		t.Skipf("test runs in the QEMU lane (TBX_E2E_HYPERVISOR=qemu); selected %s", backend.Name)
	}
	requireNoForeignClusters(t)

	name := uniqueE2EClusterName("qemu-restart")
	yaml, err := renderE2EConfig(validE2ETestConfig(name, hypervisor.NameQEMU))
	if err != nil {
		t.Fatalf("render QEMU suspend e2e config: %v", err)
	}
	configPath := writeE2EConfig(t, yaml)
	logOffset := captureTBXDLogOffset(t)
	var cleanupOutput strings.Builder
	registerE2EFailureDiagnostics(t, logOffset, &cleanupOutput)
	runTBX(t, "up", "-f", configPath)
	registerE2EClusterCleanup(t, name, &cleanupOutput)

	status, _ := waitForHypervisorLifecycleStatus(t, name, hypervisorLifecycleStatusTimeout, func(status daemon.ClusterStatus) bool {
		return lifecycleNodeRunning(status, hypervisor.NameQEMU)
	})
	nodeName := status.Nodes[0].Name

	runTBX(t, "cluster", "suspend", name)
	// Leave a full clock-drift reporting interval after the save timestamp.
	time.Sleep(6 * time.Second)
	waitForHypervisorLifecycleStatus(t, name, hypervisorLifecycleStatusTimeout, func(status daemon.ClusterStatus) bool {
		return status.Hypervisor == hypervisor.NameQEMU && !status.Running && status.Suspended &&
			len(status.Nodes) == 1 && status.Nodes[0].Suspended && status.Nodes[0].Phase == daemon.PhaseSuspended
	})

	// Add no command-scoped environment override: the replacement daemon must
	// use the cluster's persisted QEMU selection, not TBX_HYPERVISOR.
	runTBX(t, "system", "restart")
	_, restartedStatus := waitForHypervisorLifecycleStatus(t, name, hypervisorLifecycleStatusTimeout, func(status daemon.ClusterStatus) bool {
		return status.Hypervisor == hypervisor.NameQEMU && !status.Running && status.Suspended &&
			len(status.Nodes) == 1 && status.Nodes[0].Phase == daemon.PhaseSuspended
	})
	if strings.Contains(restartedStatus, "will cold-boot") {
		t.Fatalf("status after daemon restart says the QEMU save will cold-boot:\n%s", restartedStatus)
	}

	resumeOutput := runTBX(t, "cluster", "resume", name)
	if !strings.Contains(resumeOutput, "guest clocks resume about") {
		t.Fatalf("resume output = %q, want restored-clock evidence", resumeOutput)
	}
	if strings.Contains(resumeOutput, "cold-boot") {
		t.Fatalf("resume output = %q, want no cold-boot warning", resumeOutput)
	}
	waitForHypervisorLifecycleStatus(t, name, hypervisorLifecycleStatusTimeout, func(status daemon.ClusterStatus) bool {
		return lifecycleNodeRunning(status, hypervisor.NameQEMU) && status.Nodes[0].Name == nodeName
	})

	console := runTBX(t, "console", name, nodeName, "--no-follow", "--lines", "300")
	// The bounded console tail can retain the original kernel banner; a second
	// banner would prove resume cold-booted instead of restoring saved memory.
	bootBanner := regexp.MustCompile(`(?m)Linux version \S+.*#1\b`)
	if count := len(bootBanner.FindAllString(console, -1)); count > 1 {
		t.Fatalf("console tail contains %d kernel boot banners after resume, want at most the original:\n%s", count, console)
	}
}

func TestUpRefusesHypervisorDriftE2E(t *testing.T) {
	backend := requireE2EHypervisor(t)
	name := uniqueE2EClusterName("hypervisor-drift")
	yaml, err := renderE2EConfig(validE2ETestConfig(name, backend.Name))
	if err != nil {
		t.Fatalf("render hypervisor drift e2e config: %v", err)
	}
	configPath := writeE2EConfig(t, yaml)
	logOffset := captureTBXDLogOffset(t)
	var cleanupOutput strings.Builder
	registerE2EFailureDiagnostics(t, logOffset, &cleanupOutput)
	runTBX(t, "up", "-f", configPath)
	registerE2EClusterCleanup(t, name, &cleanupOutput)
	waitForHypervisorLifecycleStatus(t, name, hypervisorLifecycleStatusTimeout, func(status daemon.ClusterStatus) bool {
		return lifecycleNodeRunning(status, backend.Name)
	})

	newBackend := hypervisor.NameQEMU
	if backend.Name == hypervisor.NameQEMU {
		newBackend = hypervisor.NameVZ
	}
	driftYAML, err := renderE2EConfig(validE2ETestConfig(name, newBackend))
	if err != nil {
		t.Fatalf("render changed-hypervisor e2e config: %v", err)
	}
	driftPath := writeE2EConfig(t, driftYAML)
	output := runTBXFailure(t, "up", "-f", driftPath)
	expected := fmt.Sprintf(
		"cluster %q: hypervisor is immutable (cluster has %q, talosbox.yaml wants %q); destroy and recreate the cluster to change the hypervisor",
		name, backend.Name, newBackend,
	)
	if !strings.Contains(output, expected) {
		t.Fatalf("drift refusal output = %q, want it to contain %q", output, expected)
	}

	waitForHypervisorLifecycleStatus(t, name, hypervisorLifecycleStatusTimeout, func(status daemon.ClusterStatus) bool {
		return lifecycleNodeRunning(status, backend.Name)
	})
}

func waitForHypervisorLifecycleStatus(t *testing.T, name string, limit time.Duration, ready func(daemon.ClusterStatus) bool) (daemon.ClusterStatus, string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	lastObservation := "status was not attempted"
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out after %s waiting for cluster %q status: %s", limit, name, lastObservation)
		}
		probeTimeout := 30 * time.Second
		if remaining < probeTimeout {
			probeTimeout = remaining
		}
		output, err := runTBXCommand(t, nil, probeTimeout, "status", name, "-o", "json")
		if err != nil {
			lastObservation = fmt.Sprintf("command failed: %v\n%s", err, output)
		} else {
			var statuses []daemon.ClusterStatus
			if err := json.Unmarshal([]byte(output), &statuses); err != nil {
				lastObservation = fmt.Sprintf("decode failed: %v\n%s", err, output)
			} else if len(statuses) != 1 || statuses[0].Name != name {
				lastObservation = fmt.Sprintf("unexpected status payload: %s", output)
			} else if ready(statuses[0]) {
				return statuses[0], output
			} else {
				lastObservation = output
			}
		}
		time.Sleep(time.Second)
	}
}

func lifecycleNodeRunning(status daemon.ClusterStatus, backend hypervisor.Name) bool {
	if status.Hypervisor != backend || !status.Running || len(status.Nodes) != 1 {
		return false
	}
	switch status.Nodes[0].Phase {
	case daemon.PhaseMaintenance, daemon.PhaseConfigured, daemon.PhaseRebooted:
		return true
	default:
		return false
	}
}
