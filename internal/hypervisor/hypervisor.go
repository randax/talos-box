// Package hypervisor defines the platform-neutral virtual-machine boundary.
package hypervisor

import (
	"context"
	"errors"

	"github.com/randax/talos-box/internal/helper"
)

var (
	// ErrUnsupported reports a feature or backend unavailable on this host.
	ErrUnsupported = errors.New("hypervisor feature unsupported")
	// ErrDeviceNotActive reports an operation that requires a running device.
	ErrDeviceNotActive = errors.New("hypervisor device is not active")
	// ErrIncompatibleSave reports saved state that cannot be restored by the
	// current machine configuration or backend.
	ErrIncompatibleSave = errors.New("hypervisor saved state is incompatible")
)

// FeatureStatus describes whether a runtime-conditional feature is usable.
type FeatureStatus struct {
	Supported bool
	Reason    string
}

// Capabilities is the set of optional features detected for a backend.
type Capabilities struct {
	Suspend FeatureStatus
	// SuspendSurvivesDaemonRestart reports whether a saved state outlives the
	// process that wrote it. QEMU restores from the versioned file alone, so a
	// save survives a daemon restart; vz restore needs the file-handle-backed
	// device identity of the writing process, so it does not. Status uses this
	// to decide whether replacing the daemon has already lost the memory.
	SuspendSurvivesDaemonRestart bool
	BalloonReadback              FeatureStatus
	// GuestAgent reports whether the backend can expose the QEMU guest-agent
	// channel a machine needs for the qemu-guest-agent extension to do anything.
	GuestAgent FeatureStatus
}

// Architecture is the machine architecture produced by a hypervisor backend.
type Architecture string

const (
	ArchitectureAMD64 Architecture = "amd64"
	ArchitectureARM64 Architecture = "arm64"
)

// Restore asks Launch to restore saved state. Launch falls back to a cold boot
// and reports why through Fallback when the state is missing or incompatible.
type Restore struct {
	Path     string
	Fallback func(error)
}

// Spec contains the resources needed to create and start one machine.
// Network is lazy so an in-process restore can reuse its retained attachment;
// once invoked, ownership of the returned attachment belongs to the backend.
type Spec struct {
	CPUs              int
	MemoryMiB         int
	DiskPath          string
	MAC               string
	Network           func() (*helper.Attachment, error)
	EFIVarsPath       string
	ConsoleSocketPath string
	// GuestAgentSocketPath asks the backend to expose the guest-agent channel
	// on that path. Empty means the cluster did not request qemu-guest-agent;
	// backends without the capability ignore it, so the config stays portable.
	GuestAgentSocketPath string
	// DisableBalloon launches the guest without a memory balloon device, so
	// the host never retargets its memory (#513). The daemon sets it from
	// TBX_DISABLE_BALLOON; a guest without the device is never ballooned.
	DisableBalloon bool
	Restore        *Restore
}

// Hypervisor creates machines and reports runtime capabilities.
type Hypervisor interface {
	Launch(context.Context, Spec) (Machine, error)
	Capabilities() Capabilities
	Architecture() Architecture
}

// Machine is a running or retained virtual machine.
type Machine interface {
	Active() bool
	SetMemoryTargetMiB(int) error
	Stop(context.Context) error
	Suspend(context.Context, string) error
	Close() error
}
