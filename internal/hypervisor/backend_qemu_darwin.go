//go:build darwin

package hypervisor

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
)

func newQEMU(ctx context.Context) (Hypervisor, error) {
	return newQEMUWith(ctx, defaultQEMUDarwinProbeDeps())
}

func newQEMUWith(ctx context.Context, deps qemuDarwinProbeDeps) (Hypervisor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	architecture := Architecture(runtime.GOARCH)
	system, err := qemuSystemForArchitecture(architecture)
	if err != nil {
		return nil, err
	}
	binary, err := deps.lookPath(system.Binary)
	if err != nil {
		return nil, newUnavailableError(system.Binary+" was not found on PATH", qemuDarwinInstallRemediation, err)
	}
	accelerators, err := deps.accelHelp(ctx, binary)
	if err != nil {
		return nil, newUnavailableError("QEMU does not list the hvf accelerator", qemuDarwinHVFBuildRemediation, err)
	}
	if !hasDarwinQEMUAccelerator(accelerators, "hvf") {
		return nil, newUnavailableError("QEMU does not list the hvf accelerator", qemuDarwinHVFBuildRemediation, nil)
	}
	hvSupport, err := deps.sysctl("kern.hv_support")
	if err != nil || hvSupport != 1 {
		return nil, newUnavailableError("HVF denied: kern.hv_support is not 1", qemuDarwinHostRemediation, err)
	}
	entitled, err := deps.entitled(ctx, binary)
	if err != nil || !entitled {
		return nil, newUnavailableError("HVF denied: "+binary+" lacks com.apple.security.hypervisor", qemuDarwinEntitlementRemediation, err)
	}
	probe, err := deps.probe(ctx, binary, verifyQMPPeerDarwin)
	if err != nil {
		return nil, fmt.Errorf("probe QEMU: %w", err)
	}
	if err := validateQEMUProbe(probe, system.Machine); err != nil {
		return nil, err
	}
	resolvedBinary, err := deps.evalSymlinks(binary)
	if err != nil {
		return nil, fmt.Errorf("resolve QEMU binary path: %w", err)
	}
	homeDir, _ := deps.homeDir()
	candidates := darwinQEMUFirmwareCandidates(resolvedBinary, homeDir, os.Getenv("USER"), architecture)
	firmware, err := discoverQEMUFirmware(deps.fs, architecture, candidates)
	if err != nil {
		reason := fmt.Sprintf("no matching EFI firmware pair found for %s", architecture)
		return nil, newUnavailableError(reason, qemuDarwinFirmwareRemediation, err)
	}
	if architecture == ArchitectureARM64 {
		system.Machine = "virt,gic-version=3"
	}
	return &qemuHypervisor{
		architecture: architecture,
		system:       system,
		binary:       binary,
		accelerator:  "hvf",
		cpu:          "host",
		firmware:     firmware,
		version:      probe.Version,
		capabilities: qemuCapabilities(probe.Version),
		newConsole:   newQEMUConsoleProxy,
		verifyPeer:   verifyQMPPeerDarwin,
		saved:        make(map[string]*qemuMachine),
	}, nil
}

func hasDarwinQEMUAccelerator(output []byte, accelerator string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == accelerator {
			return true
		}
	}
	return false
}
