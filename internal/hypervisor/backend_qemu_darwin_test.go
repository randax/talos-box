//go:build darwin

package hypervisor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/randax/talos-box/internal/helper"
)

func TestDarwinQEMUBackendUsesHVFConfiguration(t *testing.T) {
	t.Parallel()

	backend := mustDarwinQEMUBackend(t)
	wantMachine := "q35"
	if Architecture(runtime.GOARCH) == ArchitectureARM64 {
		wantMachine = "virt,gic-version=3"
	}
	if backend.architecture != Architecture(runtime.GOARCH) || backend.system.Machine != wantMachine || backend.accelerator != "hvf" || backend.cpu != "host" {
		t.Fatalf("backend configuration = architecture %q machine %q accelerator %q CPU %q", backend.architecture, backend.system.Machine, backend.accelerator, backend.cpu)
	}
	if backend.newConsole == nil || backend.verifyPeer == nil {
		t.Fatal("backend did not install the shared console factory and Darwin peer verifier")
	}
}

func TestDarwinQEMUBackendReportsBalloonAndRestartSafeSuspend(t *testing.T) {
	t.Parallel()

	capabilities := mustDarwinQEMUBackend(t).Capabilities()
	if !capabilities.BalloonReadback.Supported || !capabilities.Suspend.Supported || !capabilities.SuspendSurvivesDaemonRestart {
		t.Fatalf("capabilities = %+v, want balloon and restart-safe suspend", capabilities)
	}
}

func TestDarwinQEMUBackendAcceptsDatagramAttachment(t *testing.T) {
	t.Parallel()

	backend := mustDarwinQEMUBackend(t)
	dir := t.TempDir()
	backend.firmware.VarsPath = filepath.Join(dir, "edk2-vars.fd")
	writeFile(t, backend.firmware.VarsPath, "vars")
	backend.newConsole = func(string) (*consoleProxy, *os.File, error) {
		file, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		return nil, file, err
	}
	network, peer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	machine, err := backend.newMachine(Spec{
		CPUs:              2,
		MemoryMiB:         2048,
		DiskPath:          filepath.Join(dir, "node.img"),
		MAC:               "02:00:00:00:00:01",
		EFIVarsPath:       filepath.Join(dir, "node.efi"),
		ConsoleSocketPath: filepath.Join(dir, "node.console.sock"),
		Network: func() (*helper.Attachment, error) {
			return &helper.Attachment{Kind: helper.AttachmentDatagramFD, File: network}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = machine.Close() }()
	cfg := machine.launchConfig(filepath.Join(dir, "qmp.sock"), "")
	if cfg.NetworkKind != helper.AttachmentDatagramFD || cfg.NetworkFD != 3 {
		t.Fatalf("network launch config = kind %q fd %d, want datagram fd 3", cfg.NetworkKind, cfg.NetworkFD)
	}
}

func mustDarwinQEMUBackend(t *testing.T) *qemuHypervisor {
	t.Helper()
	backend, err := newQEMUWith(context.Background(), passingDarwinQEMUProbeDeps(t, Architecture(runtime.GOARCH)))
	if err != nil {
		t.Fatal(err)
	}
	qemu, ok := backend.(*qemuHypervisor)
	if !ok {
		t.Fatalf("newQEMUWith() = %T, want *qemuHypervisor", backend)
	}
	return qemu
}
