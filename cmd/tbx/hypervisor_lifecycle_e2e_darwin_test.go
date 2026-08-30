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
	daemonEnv := captureE2EDaemonEnvState(t)
	if daemonEnv.hypervisorEnv != "" || !daemonEnv.reserveDefault {
		// The mid-test restart deliberately clears TBX_HYPERVISOR (the proof
		// needs the stored cluster state to win, not an env default), so a
		// daemon started with overrides must get them back afterwards. LIFO:
		// registered first, this runs after the owned cluster is destroyed.
		t.Cleanup(func() {
			output, err := runTBXCommand(t, daemonEnv.env(), e2eCleanupTimeout, "system", "restart", "--force")
			if err != nil {
				t.Errorf("restore tbxd daemon environment: %v\n%s", err, output)
			}
		})
	}

	name := uniqueE2EClusterName("qemu-restart")
	yaml, err := renderE2EConfig(validE2ETestConfig(name, hypervisor.NameQEMU))
	if err != nil {
		t.Fatalf("render QEMU suspend e2e config: %v", err)
	}
	configPath := writeE2EConfig(t, yaml)
	logOffset := captureTBXDLogOffset(t)
	var cleanupOutput strings.Builder
	registerE2EClusterCleanup(t, name, &cleanupOutput)
	registerE2EFailureDiagnostics(t, logOffset)
	runTBX(t, "up", "-f", configPath)

	status, _ := waitForHypervisorLifecycleStatus(t, name, hypervisorLifecycleStatusTimeout, func(status daemon.ClusterStatus) bool {
		return lifecycleNodeRunning(status, hypervisor.NameQEMU)
	})
	nodeName := status.Nodes[0].Name
	bootBanner := regexp.MustCompile(`(?m)Linux version \S+.*#1\b`)

	runTBX(t, "cluster", "suspend", name)
	// Leave a full clock-drift reporting interval after the save timestamp.
	time.Sleep(6 * time.Second)
	waitForHypervisorLifecycleStatus(t, name, hypervisorLifecycleStatusTimeout, func(status daemon.ClusterStatus) bool {
		return status.Hypervisor == hypervisor.NameQEMU && !status.Running && status.Suspended &&
			len(status.Nodes) == 1 && status.Nodes[0].Suspended && status.Nodes[0].Phase == daemon.PhaseSuspended
	})

	// Clear TBX_HYPERVISOR — ambient or carried from the daemon's own
	// environment — so the persisted per-cluster QEMU selection must win on
	// its own; keep the captured balloon reserve so only that one variable
	// differs from the daemon being replaced.
	runTBXWithEnv(t, daemonEnv.env("TBX_HYPERVISOR="), "system", "restart")
	_, restartedStatus := waitForHypervisorLifecycleStatus(t, name, hypervisorLifecycleStatusTimeout, func(status daemon.ClusterStatus) bool {
		return status.Hypervisor == hypervisor.NameQEMU && !status.Running && status.Suspended &&
			len(status.Nodes) == 1 && status.Nodes[0].Phase == daemon.PhaseSuspended
	})
	if strings.Contains(restartedStatus, "will cold-boot") {
		t.Fatalf("status after daemon restart says the QEMU save will cold-boot:\n%s", restartedStatus)
	}

	// Baseline the boot-banner count from the REPLACEMENT daemon's console
	// buffer, so both reads come from the same proxy lifetime: a cold boot
	// during resume then adds exactly one banner and is caught, where a
	// pre-suspend baseline from the old daemon's buffer would mask it. The
	// read may fail while the node is suspended; treat that as an empty
	// buffer.
	preResumeConsole, preResumeErr := runTBXCommand(t, nil, e2eCleanupTimeout, "console", name, nodeName, "--no-follow", "--lines", "300")
	preResumeBannerCount := 0
	if preResumeErr == nil {
		preResumeBannerCount = len(bootBanner.FindAllString(preResumeConsole, -1))
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

	postResumeConsole := runTBX(t, "console", name, nodeName, "--no-follow", "--lines", "300")
	postResumeBannerCount := len(bootBanner.FindAllString(postResumeConsole, -1))
	if postResumeBannerCount > preResumeBannerCount {
		t.Fatalf("kernel boot banner appeared during resume (cold boot): before=%d after=%d\npre-resume console tail:\n%s\npost-resume console tail:\n%s", preResumeBannerCount, postResumeBannerCount, preResumeConsole, postResumeConsole)
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
	registerE2EClusterCleanup(t, name, &cleanupOutput)
	registerE2EFailureDiagnostics(t, logOffset)
	runTBX(t, "up", "-f", configPath)
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
	containsExpected := strings.Contains(output, expected)
	var immutableLine string
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "hypervisor is immutable") {
			immutableLine = line
			break
		}
	}
	if immutableLine == "" {
		t.Fatalf("drift refusal output contains no line with %q (contains full expected text: %t):\n%s", "hypervisor is immutable", containsExpected, output)
	}
	actual := strings.TrimSpace(immutableLine)
	actual = strings.TrimSpace(strings.TrimPrefix(actual, "tbx: "))
	if actual != expected {
		t.Fatalf("drift refusal line = %q, want exactly %q (full output contains expected text: %t):\n%s", actual, expected, containsExpected, output)
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
