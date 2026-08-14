package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	}
	if version.Compare(qemuSuspendVersion) >= 0 {
		capabilities.Suspend.Supported = true
	} else {
		capabilities.Suspend.Reason = fmt.Sprintf(
			"suspend requires QEMU >= %d.%d (found %d.%d) — upgrade to Ubuntu 24.04+",
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
