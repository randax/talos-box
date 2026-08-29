//go:build !darwin

package hostpressure

import "context"

// SystemSnapshot reports that host-pressure measurement is not yet available
// on this platform. Linux diagnostics are tracked separately by issue #93.
func SystemSnapshot(string) (Snapshot, error) {
	return Snapshot{}, ErrUnsupported
}

func SystemMemorySnapshot() (Snapshot, error) { return Snapshot{}, ErrUnsupported }

func SystemMemorySnapshotContext(context.Context) (Snapshot, error) {
	return Snapshot{}, ErrUnsupported
}
