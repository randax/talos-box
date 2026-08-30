//go:build e2e && darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/hypervisor"
)

func TestDoctorHypervisorsE2E(t *testing.T) {
	selected := requireE2EHypervisor(t)
	output, doctorErr := runTBXCommand(t, nil, e2eCommandTimeout, "doctor")
	inventory, err := parseDoctorHypervisorInventory(output, doctorErr)
	if err != nil {
		t.Fatalf("parse `tbx doctor` hypervisor inventory: %v\nfull doctor output:\n%s", err, output)
	}

	names := make([]string, 0, len(inventory.Backends))
	for name := range inventory.Backends {
		names = append(names, string(name))
	}
	sort.Strings(names)
	var expectedNames []string
	switch runtime.GOARCH {
	case "arm64":
		expectedNames = []string{"qemu", "vz"}
	case "amd64":
		expectedNames = []string{"qemu"}
	default:
		t.Fatalf("unexpected Darwin architecture %q", runtime.GOARCH)
	}
	if strings.Join(names, ",") != strings.Join(expectedNames, ",") {
		t.Fatalf("doctor hypervisor backend names = %v, want exactly %v on darwin/%s", names, expectedNames, runtime.GOARCH)
	}
	previousIndex := -1
	for _, name := range names {
		prefix := doctorHypervisorPrefix + name + ":"
		if count := strings.Count(output, prefix); count != 1 {
			t.Fatalf("doctor output contains %d inventory lines for %q, want exactly one:\n%s", count, name, output)
		}
		index := strings.Index(output, prefix)
		if index <= previousIndex {
			t.Fatalf("doctor hypervisor inventory is not lexically ordered by backend name:\n%s", output)
		}
		previousIndex = index

		entry := inventory.Backends[hypervisor.Name(name)]
		if len(entry.Capabilities) != 4 {
			t.Fatalf("doctor hypervisor %q has %d capabilities, want four: %+v", name, len(entry.Capabilities), entry.Capabilities)
		}
		for _, capability := range []string{"balloon-readback", "suspend", "suspend-survives-restart", "guest-agent"} {
			if _, ok := entry.Capabilities[capability]; !ok {
				t.Fatalf("doctor hypervisor %q is missing capability %q: %s", name, capability, entry.Raw)
			}
		}
	}

	defaultEntry := inventory.Backends[inventory.Default]
	if !defaultEntry.Default {
		t.Fatalf("doctor inventory default %q is not marked default=yes: %+v", inventory.Default, defaultEntry)
	}
	if defaultEntry.DefaultSource != string(hypervisor.DefaultSourceCompiled) && defaultEntry.DefaultSource != hypervisor.DefaultEnv {
		t.Fatalf("doctor inventory default source = %q, want %q or %q", defaultEntry.DefaultSource, hypervisor.DefaultSourceCompiled, hypervisor.DefaultEnv)
	}
	selectedEntry, ok := inventory.Backends[selected.Name]
	if !ok {
		t.Fatalf("doctor inventory lost selected backend %q: %+v", selected.Name, inventory.Backends)
	}
	if !selectedEntry.Available {
		t.Fatalf("selected backend %q is unavailable: %s; remediation: %s", selected.Name, selectedEntry.Reason, selectedEntry.Remediation)
	}

	if selected.Name == hypervisor.NameQEMU {
		qemu := inventory.Backends[hypervisor.NameQEMU]
		for _, capability := range []string{"balloon-readback", "suspend", "suspend-survives-restart", "guest-agent"} {
			if got := qemu.Capabilities[capability]; got != "supported" {
				t.Errorf("QEMU capability %q = %q, want supported; line: %s", capability, got, qemu.Raw)
			}
		}
	}
	if selected.Name == hypervisor.NameVZ {
		if qemu, present := inventory.Backends[hypervisor.NameQEMU]; present && !qemu.Available {
			if qemu.Reason == "" || qemu.Remediation == "" {
				t.Fatalf("unavailable QEMU inventory entry must include reason and remediation: %s", qemu.Raw)
			}
		}
	}
}

func TestQEMUBalloonReadbackInMaintenanceE2E(t *testing.T) {
	selected := requireE2EHypervisor(t)
	if selected.Name != hypervisor.NameQEMU {
		t.Skip("runs in the QEMU lane (TBX_E2E_HYPERVISOR=qemu)")
	}
	requireNoForeignClusters(t)
	socketPath, err := daemon.SocketPath()
	if err != nil {
		t.Fatalf("resolve daemon socket path: %v", err)
	}
	info, _, err := daemonHandshake(socketPath)
	if err != nil {
		t.Fatalf("read running daemon info from %s: %v", socketPath, err)
	}
	if info.BalloonDisabled {
		t.Fatal("ballooning is disabled on the running daemon; unset TBX_DISABLE_BALLOON and restart tbxd")
	}
	if value := os.Getenv("TBX_DISABLE_BALLOON"); value != "" {
		t.Fatalf("TBX_DISABLE_BALLOON=%q disables ballooning required by this test; unset TBX_DISABLE_BALLOON and restart tbxd", value)
	}

	originalReserveMiB := info.BalloonReserveMiB
	compiledReserveMiB := balloon.DefaultConfig().ReserveMiB
	if originalReserveMiB == 0 {
		originalReserveMiB = compiledReserveMiB
	}
	originalWasDefault := originalReserveMiB == compiledReserveMiB
	hostFreeMiB, err := balloon.HostFreeMiB()
	if err != nil {
		t.Fatalf("read host free memory: %v", err)
	}
	if hostFreeMiB <= 0 {
		t.Fatalf("host free memory = %d MiB, want a positive value", hostFreeMiB)
	}
	newReserveMiB := hostFreeMiB + 2048

	t.Cleanup(func() {
		var env []string
		if !originalWasDefault {
			env = []string{"TBX_BALLOON_RESERVE_MIB=" + strconv.Itoa(originalReserveMiB)}
		}
		output, err := runTBXCommand(t, env, e2eCleanupTimeout, "system", "restart", "--force")
		if err != nil {
			t.Errorf("restore tbxd balloon reserve to %d MiB: %v\n%s", originalReserveMiB, err, output)
		}
	})
	runTBXWithEnv(t, []string{"TBX_BALLOON_RESERVE_MIB=" + strconv.Itoa(newReserveMiB)}, "system", "restart", "--force")

	logOffset := captureTBXDLogOffset(t)
	name := uniqueE2EClusterName("balloon")
	cfg := config.Config{Clusters: []config.ClusterSpec{{
		Name:          name,
		Hypervisor:    hypervisor.NameQEMU,
		ControlPlanes: 1,
		Workers:       0,
		Node: cluster.NodeDefaults{
			MemoryMiB: 4096,
			CPUs:      cluster.DefaultCPUs,
			DiskGiB:   cluster.DefaultDiskGiB,
		},
	}}}
	yaml, err := renderE2EConfig(cfg)
	if err != nil {
		t.Fatalf("render balloon e2e config: %v", err)
	}
	configPath := writeE2EConfig(t, yaml)
	var cleanupOutput strings.Builder
	registerE2EClusterCleanup(t, name, &cleanupOutput)
	registerE2EFailureDiagnostics(t, logOffset)
	runTBX(t, "up", "--force", "-f", configPath)

	// A node that just booted reads "unreachable" until Talos's maintenance
	// apid answers, so the phase needs a bounded wait, not a one-shot read.
	var status *daemon.ClusterStatus
	deadline := time.Now().Add(3 * time.Minute)
	for {
		statusOutput := runTBX(t, "status", "-o", "json")
		var statuses []daemon.ClusterStatus
		if err := json.Unmarshal([]byte(statusOutput), &statuses); err != nil {
			t.Fatalf("decode `tbx status -o json`: %v\n%s", err, statusOutput)
		}
		status = nil
		for index := range statuses {
			if statuses[index].Name == name {
				status = &statuses[index]
				break
			}
		}
		if status == nil {
			t.Fatalf("`tbx status -o json` is missing cluster %q: %s", name, statusOutput)
		}
		if status.Hypervisor != hypervisor.NameQEMU {
			t.Fatalf("cluster %q hypervisor = %q, want qemu", name, status.Hypervisor)
		}
		if len(status.Nodes) != 1 {
			t.Fatalf("cluster %q has %d nodes, want exactly one: %+v", name, len(status.Nodes), status.Nodes)
		}
		if status.Nodes[0].Phase == daemon.PhaseMaintenance {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster %q node %q phase = %q, want %q after %s", name, status.Nodes[0].Name, status.Nodes[0].Phase, daemon.PhaseMaintenance, 3*time.Minute)
		}
		time.Sleep(5 * time.Second)
	}

	nodeName := status.Nodes[0].Name
	pattern := regexp.MustCompile(fmt.Sprintf(
		`balloon %s/%s: target=(\d+)MiB \(configured=4096 hostFree=-?\d+ reserve=\d+ deficit=\d+`,
		regexp.QuoteMeta(name), regexp.QuoteMeta(nodeName),
	))
	logTail := waitForTBXDLog(t, logOffset, pattern, 7*time.Minute)
	match := pattern.FindStringSubmatch(logTail)
	if match == nil {
		t.Fatalf("matched tbxd.log tail no longer contains %q:\n%s", pattern, logTail)
	}
	targetMiB, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parse balloon target %q for %s/%s: %v", match[1], name, nodeName, err)
	}
	if targetMiB < 1024 || targetMiB >= 4096 {
		t.Fatalf("balloon target for %s/%s = %d MiB, want 1024 <= target < 4096; log: %s", name, nodeName, targetMiB, match[0])
	}
}
