//go:build e2e && darwin

package main

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/hypervisor"
)

const (
	mixedE2EReadyTimeout = 25 * time.Minute
	mixedE2EPollInterval = 15 * time.Second
)

func TestMixedHypervisorsInterClusterE2E(t *testing.T) {
	requireE2ERuntime(t)
	lane := requireE2EHypervisor(t)
	if lane.Name == hypervisor.NameVZ {
		t.Skip("runs in the QEMU lane (TBX_E2E_HYPERVISOR=qemu)")
	}
	if lane.Name != hypervisor.NameQEMU {
		t.Fatalf("mixed inter-cluster e2e requires the qemu lane, got %q", lane.Name)
	}
	requireMixedE2EVZ(t)
	if runtime.GOARCH == "amd64" {
		t.Skip("mixed VZ+QEMU topology is unavailable on Intel Darwin; Apple Silicon is required")
	}
	requireNoForeignClusters(t)

	vzName := uniqueE2EClusterName("mixed-vz")
	qemuName := uniqueE2EClusterName("mixed-qemu")
	intent := cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}
	node := cluster.NodeDefaults{MemoryMiB: 2048, CPUs: cluster.DefaultCPUs, DiskGiB: 10}
	cfg := config.Config{Clusters: []config.ClusterSpec{
		{
			Name:               vzName,
			Hypervisor:         hypervisor.NameVZ,
			ControlPlanes:      1,
			Workers:            0,
			ProvisioningIntent: intent,
			Node:               node,
		},
		{
			Name:               qemuName,
			Hypervisor:         hypervisor.NameQEMU,
			ControlPlanes:      1,
			Workers:            0,
			ProvisioningIntent: intent,
			Node:               node,
		},
	}}
	yaml, err := renderE2EConfig(cfg)
	if err != nil {
		t.Fatalf("render mixed hypervisor e2e config: %v", err)
	}
	configPath := writeE2EConfig(t, yaml)

	logOffset := captureTBXDLogOffset(t)
	var cleanupOutput strings.Builder
	registerE2EFailureDiagnostics(t, logOffset, &cleanupOutput)
	readyDeadline := time.Now().Add(mixedE2EReadyTimeout)
	upOutput, upErr := runTBXCommand(t, nil, mixedE2EReadyTimeout, "up", "--force", "-f", configPath)
	registerE2EClusterCleanup(t, vzName, &cleanupOutput)
	registerE2EClusterCleanup(t, qemuName, &cleanupOutput)
	if upErr != nil {
		t.Fatalf("tbx up --force -f %s: %v\n%s", configPath, upErr, upOutput)
	}

	waitForMixedE2EClusters(t, readyDeadline, map[string]hypervisor.Name{
		vzName:   hypervisor.NameVZ,
		qemuName: hypervisor.NameQEMU,
	})

	const want = "PASS inter-cluster: 2 cluster VIP(s) reachable from the host and from each sibling"
	doctorOutput := runTBX(t, "doctor")
	if !strings.Contains(doctorOutput, want) {
		t.Fatalf("`tbx doctor` output does not contain %q:\n%s", want, doctorOutput)
	}
}

func requireMixedE2EVZ(t *testing.T) {
	t.Helper()
	output, doctorErr := runTBXCommand(t, nil, e2eCommandTimeout, "doctor")
	inventory, err := parseDoctorHypervisorInventory(output, doctorErr)
	if err != nil {
		t.Fatalf("parse `tbx doctor` hypervisor inventory for VZ: %v\nfull doctor output:\n%s", err, output)
	}
	if _, err := selectedE2EHypervisor(inventory, hypervisor.NameVZ); err != nil {
		var unavailable e2eUnavailableError
		if errors.As(err, &unavailable) {
			detail := unavailable.entry.Reason
			if unavailable.entry.Remediation != "" {
				detail += "; remediation: " + unavailable.entry.Remediation
			}
			t.Skipf("VZ is required for the mixed inter-cluster test and is unavailable: %s", detail)
		}
		t.Fatalf("select VZ for mixed inter-cluster e2e: %v\nfull doctor output:\n%s", err, output)
	}
}

func waitForMixedE2EClusters(t *testing.T, deadline time.Time, expected map[string]hypervisor.Name) {
	t.Helper()
	var lastOutput string
	var lastErr error
	for {
		lastOutput, lastErr = runTBXCommand(t, nil, 30*time.Second, "status", "-o", "json")
		if lastErr == nil {
			var statuses []daemon.ClusterStatus
			if err := json.Unmarshal([]byte(lastOutput), &statuses); err != nil {
				lastErr = err
			} else if mixedE2EClustersReady(statuses, expected) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for mixed clusters to become ready; last status error: %v\nlast `tbx status -o json` output:\n%s", lastErr, lastOutput)
		}
		time.Sleep(mixedE2EPollInterval)
	}
}

func mixedE2EClustersReady(statuses []daemon.ClusterStatus, expected map[string]hypervisor.Name) bool {
	ready := make(map[string]bool, len(expected))
	for _, status := range statuses {
		backend, ok := expected[status.Name]
		if !ok {
			continue
		}
		ready[status.Name] = status.Hypervisor == backend && status.Running && status.KubernetesReady &&
			status.VIPLive && status.VIP != ""
	}
	for name := range expected {
		if !ready[name] {
			return false
		}
	}
	return true
}
