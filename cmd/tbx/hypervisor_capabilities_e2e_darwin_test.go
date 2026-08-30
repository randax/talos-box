//go:build e2e && darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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

var doctorHostPressureNumbers = regexp.MustCompile(
	`(?m)^(?:PASS|WARN|FAIL) host-pressure: (?:(\d+) MiB free memory[^\n]*leave the (\d+) MiB balloon reserve free|[^\n]*with (\d+) MiB free memory against (\d+) MiB required)`,
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
	if value := os.Getenv("TBX_DISABLE_BALLOON"); value != "" {
		t.Fatalf("TBX_DISABLE_BALLOON=%q disables ballooning required by this test; unset TBX_DISABLE_BALLOON and restart tbxd", value)
	}

	doctorOutput, doctorErr := runTBXCommand(t, nil, e2eCommandTimeout, "doctor")
	matches := doctorHostPressureNumbers.FindStringSubmatch(doctorOutput)
	if matches == nil {
		t.Fatalf("parse daemon balloon reserve and host free memory from `tbx doctor` host-pressure finding (doctor error: %v):\n%s", doctorErr, doctorOutput)
	}
	freeText, reserveText := matches[1], matches[2]
	if freeText == "" {
		freeText, reserveText = matches[3], matches[4]
	}
	hostFreeMiB, err := strconv.Atoi(freeText)
	if err != nil || hostFreeMiB <= 0 {
		t.Fatalf("parse host free memory %q from `tbx doctor`: %v\n%s", freeText, err, doctorOutput)
	}
	originalReserveMiB, err := strconv.Atoi(reserveText)
	if err != nil || originalReserveMiB <= 0 {
		t.Fatalf("parse balloon reserve %q from `tbx doctor`: %v\n%s", reserveText, err, doctorOutput)
	}
	compiledReserveMiB := balloon.DefaultConfig().ReserveMiB
	originalWasDefault := originalReserveMiB == compiledReserveMiB
	newReserveMiB := hostFreeMiB + 2048
	runTBXWithEnv(t, []string{"TBX_BALLOON_RESERVE_MIB=" + strconv.Itoa(newReserveMiB)}, "system", "restart", "--force")

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
	registerE2EFailureDiagnostics(t, logOffset, &cleanupOutput)
	runTBX(t, "up", "--force", "-f", configPath)
	registerE2EClusterCleanup(t, name, &cleanupOutput)

	statusOutput := runTBX(t, "status", "-o", "json")
	var statuses []daemon.ClusterStatus
	if err := json.Unmarshal([]byte(statusOutput), &statuses); err != nil {
		t.Fatalf("decode `tbx status -o json`: %v\n%s", err, statusOutput)
	}
	var status *daemon.ClusterStatus
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
	if status.Nodes[0].Phase != daemon.PhaseMaintenance {
		t.Fatalf("cluster %q node %q phase = %q, want %q", name, status.Nodes[0].Name, status.Nodes[0].Phase, daemon.PhaseMaintenance)
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
