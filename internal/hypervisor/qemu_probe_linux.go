//go:build linux

package hypervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func probeQEMU(ctx context.Context, binary string) (qemuProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	dir, err := os.MkdirTemp("/tmp", "tbx-qmp-probe-")
	if err != nil {
		return qemuProbe{}, fmt.Errorf("create QEMU probe directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	socketPath := filepath.Join(dir, "qmp.sock")
	command := exec.Command(binary,
		"-machine", "none",
		"-nodefaults",
		"-display", "none",
		"-qmp", "unix:"+socketPath+",server=on,wait=off",
	)
	var stderr strings.Builder
	command.Stderr = &stderr
	process, err := startQEMUProcess(command)
	if err != nil {
		return qemuProbe{}, fmt.Errorf("start QEMU capability probe: %w", err)
	}
	defer func() {
		_ = process.kill()
		<-process.done
	}()

	connection, err := dialQMPSocket(ctx, socketPath, process)
	if err != nil {
		return qemuProbe{}, fmt.Errorf("connect QEMU capability probe (%s): %w", strings.TrimSpace(stderr.String()), err)
	}
	if err := verifyQMPPeer(connection, process); err != nil {
		_ = connection.Close()
		return qemuProbe{}, fmt.Errorf("authenticate QEMU capability probe: %w", err)
	}
	client, err := newQMPClient(ctx, connection)
	if err != nil {
		_ = connection.Close()
		return qemuProbe{}, fmt.Errorf("handshake QEMU capability probe: %w", err)
	}
	defer func() { _ = client.close() }()

	var versionResponse struct {
		QEMU struct {
			Major int `json:"major"`
			Minor int `json:"minor"`
			Micro int `json:"micro"`
		} `json:"qemu"`
	}
	if err := client.execute(ctx, "query-version", nil, &versionResponse); err != nil {
		return qemuProbe{}, fmt.Errorf("query QEMU version: %w", err)
	}
	var machineResponse []struct {
		Name  string `json:"name"`
		Alias string `json:"alias"`
	}
	if err := client.execute(ctx, "query-machines", nil, &machineResponse); err != nil {
		return qemuProbe{}, fmt.Errorf("query QEMU machines: %w", err)
	}
	probe := qemuProbe{
		Version: qemuVersion{
			Major: versionResponse.QEMU.Major,
			Minor: versionResponse.QEMU.Minor,
			Patch: versionResponse.QEMU.Micro,
		},
		Machines: make([]string, 0, len(machineResponse)),
	}
	for _, machine := range machineResponse {
		probe.Machines = append(probe.Machines, machine.Name)
		if machine.Alias != "" {
			probe.MachineAliases = append(probe.MachineAliases, machine.Alias)
		}
	}
	// QEMU normally replies before exiting. Treat EOF after quit as success too:
	// the probe has already collected everything it needs.
	_ = client.execute(ctx, "quit", nil, nil)
	return probe, nil
}
