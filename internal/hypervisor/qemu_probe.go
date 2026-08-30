package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	qemuMinimumVersion = qemuVersion{Major: 6, Minor: 2}
	qemuSuspendVersion = qemuVersion{Major: 8, Minor: 2}
)

type qemuProbe struct {
	Version        qemuVersion
	Machines       []string
	MachineAliases []string
}

type qemuPeerVerifier func(net.Conn, *qemuProcess) error

func probeQEMU(ctx context.Context, binary string, verifyPeer qemuPeerVerifier) (qemuProbe, error) {
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
	if verifyPeer != nil {
		if err := verifyPeer(connection, process); err != nil {
			_ = connection.Close()
			return qemuProbe{}, fmt.Errorf("authenticate QEMU capability probe: %w", err)
		}
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

func validateQEMUProbe(probe qemuProbe, requiredMachine string) error {
	if probe.Version.Compare(qemuMinimumVersion) < 0 {
		return fmt.Errorf("%w: QEMU >= %d.%d is required (found %s)", ErrUnsupported, qemuMinimumVersion.Major, qemuMinimumVersion.Minor, probe.Version)
	}
	for _, machine := range probe.Machines {
		if machine == requiredMachine {
			return nil
		}
	}
	for _, alias := range probe.MachineAliases {
		if alias == requiredMachine {
			return nil
		}
	}
	return fmt.Errorf("%w: QEMU %s does not provide required machine type %q", ErrUnsupported, probe.Version, requiredMachine)
}

func qemuCapabilities(version qemuVersion) Capabilities {
	capabilities := Capabilities{
		BalloonReadback: FeatureStatus{Supported: true},
		GuestAgent:      FeatureStatus{Supported: true},
	}
	if version.Compare(qemuSuspendVersion) >= 0 {
		capabilities.Suspend.Supported = true
		// The save is a versioned file Launch feeds back in as -incoming; no
		// handle from the writing process is involved, so a daemon restart
		// costs the memory nothing.
		capabilities.SuspendSurvivesDaemonRestart = true
	} else {
		capabilities.Suspend.Reason = fmt.Sprintf(
			"suspend requires QEMU >= %d.%d (found %d.%d); upgrade QEMU",
			qemuSuspendVersion.Major,
			qemuSuspendVersion.Minor,
			version.Major,
			version.Minor,
		)
	}
	return capabilities
}

func dialQMPSocket(ctx context.Context, path string, process *qemuProcess) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 100 * time.Millisecond}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := dialer.DialContext(ctx, "unix", path)
		if err == nil {
			return connection, nil
		}
		if process != nil && !process.active() {
			waitErr := process.waitError()
			if waitErr == nil {
				waitErr = errors.New("QEMU exited before opening QMP")
			}
			return nil, waitErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
