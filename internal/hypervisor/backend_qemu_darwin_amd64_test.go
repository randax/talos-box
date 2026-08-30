//go:build darwin && amd64

package hypervisor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestDarwinAMD64QEMUBackendProbe(t *testing.T) {
	t.Parallel()

	expectedSystem, err := qemuSystemForArchitecture(ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	deps := passingDarwinQEMUProbeDeps(t, ArchitectureAMD64)
	var requestedBinary string
	deps.lookPath = func(binary string) (string, error) {
		requestedBinary = binary
		return filepath.Join("/test/bin", binary), nil
	}

	backend, err := newQEMUWith(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	qemu, ok := backend.(*qemuHypervisor)
	if !ok {
		t.Fatalf("backend type = %T, want *qemuHypervisor", backend)
	}
	if requestedBinary != expectedSystem.Binary {
		t.Fatalf("requested binary = %q, want %q", requestedBinary, expectedSystem.Binary)
	}
	if qemu.Architecture() != ArchitectureAMD64 {
		t.Fatalf("architecture = %q, want %q", qemu.Architecture(), ArchitectureAMD64)
	}
	if qemu.system.Machine != expectedSystem.Machine {
		t.Fatalf("machine = %q, want %q", qemu.system.Machine, expectedSystem.Machine)
	}
	if got := filepath.Base(qemu.firmware.CodePath); got != "edk2-x86_64-code.fd" {
		t.Fatalf("firmware code = %q, want edk2-x86_64-code.fd", got)
	}
	if got := filepath.Base(qemu.firmware.VarsPath); got != "edk2-i386-vars.fd" {
		t.Fatalf("firmware vars = %q, want edk2-i386-vars.fd", got)
	}
}

func TestDarwinAMD64QEMUBackendReportsHVFHostRemediation(t *testing.T) {
	t.Parallel()

	deps := passingDarwinQEMUProbeDeps(t, ArchitectureAMD64)
	var requestedSysctl string
	deps.sysctl = func(name string) (uint32, error) {
		requestedSysctl = name
		return 0, nil
	}

	_, err := newQEMUWith(context.Background(), deps)
	var unavailable unavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("newQEMUWith() error = %v, want unavailableError", err)
	}
	if requestedSysctl != "kern.hv_support" {
		t.Fatalf("sysctl = %q, want kern.hv_support", requestedSysctl)
	}
	if unavailable.remediation != qemuDarwinHostRemediation {
		t.Fatalf("remediation = %q, want %q", unavailable.remediation, qemuDarwinHostRemediation)
	}
}
