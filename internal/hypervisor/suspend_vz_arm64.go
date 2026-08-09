//go:build darwin && arm64

package hypervisor

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"
)

func suspendFeatureStatus() FeatureStatus {
	version, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return FeatureStatus{Reason: fmt.Sprintf("cannot determine macOS version: %v", err)}
	}
	majorText, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil {
		return FeatureStatus{Reason: fmt.Sprintf("cannot parse macOS version %q", version)}
	}
	if major < 14 {
		return FeatureStatus{Reason: fmt.Sprintf("requires macOS 14 or newer (host is %s)", version)}
	}
	return FeatureStatus{Supported: true}
}

func saveVZMachine(machine *vz.VirtualMachine, path string) error {
	return machine.SaveMachineStateToPath(path)
}

func restoreVZMachine(machine *vz.VirtualMachine, path string) error {
	return machine.RestoreMachineStateFromURL(path)
}
