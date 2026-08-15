//go:build darwin && arm64

package hypervisor

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/randax/talos-box/internal/helper"
)

func TestVZLaunchOwnsAndClosesRejectedAttachment(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = write.Close() }()
	backend := &vzHypervisor{saved: make(map[string]*vzMachine)}
	spec := validTestSpec()
	spec.Network = func() (*helper.Attachment, error) {
		return &helper.Attachment{Kind: helper.AttachmentTapFD, File: read}, nil
	}

	_, err = backend.Launch(context.Background(), spec)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Launch() = %v, want ErrUnsupported", err)
	}
	if _, err := read.Stat(); err == nil {
		t.Fatal("backend did not close rejected attachment")
	}
}

func TestProbeBootLoaderHasVariableStoreAndCleansUp(t *testing.T) {
	bootLoader, storePath, cleanup, err := newProbeBootLoader()
	if err != nil {
		t.Fatalf("newProbeBootLoader() = %v", err)
	}
	t.Cleanup(cleanup)
	if bootLoader == nil {
		t.Fatal("newProbeBootLoader() returned nil boot loader")
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("probe EFI variable store missing before cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Stat(storePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe EFI variable store not removed by cleanup: %v", err)
	}
}

func TestVZLaunchValidatesBeforeAcquiringNetwork(t *testing.T) {
	backend := &vzHypervisor{saved: make(map[string]*vzMachine)}
	acquired := false
	_, err := backend.Launch(context.Background(), Spec{
		Network: func() (*helper.Attachment, error) {
			acquired = true
			return nil, nil
		},
	})
	if err == nil {
		t.Fatal("Launch() accepted invalid spec")
	}
	if acquired {
		t.Fatal("Launch() acquired a network attachment before validating the spec")
	}
}

func validTestSpec() Spec {
	return Spec{
		CPUs:              1,
		MemoryMiB:         1024,
		DiskPath:          "/tmp/node.img",
		MAC:               "02:00:00:00:00:01",
		EFIVarsPath:       "/tmp/node.efi",
		ConsoleSocketPath: "/tmp/node.console.sock",
	}
}
