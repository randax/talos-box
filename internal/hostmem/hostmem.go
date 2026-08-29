// Package hostmem provides one neutral host-memory sample for consumers that
// need to make consistent decisions from macOS vm_stat and sysctl output.
package hostmem

import "errors"

// ErrUnsupported reports that this platform has no host-memory probe.
var ErrUnsupported = errors.New("host memory reading is only implemented on macOS")

// Pressure is the kernel's current memory-pressure verdict.
type Pressure int

const (
	PressureUnknown Pressure = iota
	PressureNormal
	PressureWarning
	PressureCritical
)

// Snapshot is one internally consistent host-memory observation.
type Snapshot struct {
	TotalMiB           int
	AvailableMiB       int
	CompressorMiB      int
	SwapTotalBytes     uint64
	SwapAvailableBytes uint64
	Pressure           Pressure
}

// SystemSnapshot is a package seam because the live implementation invokes
// host utilities. Package tests pin it to a deterministic sample in TestMain.
var SystemSnapshot = systemSnapshot
