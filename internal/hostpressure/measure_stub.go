//go:build !darwin

package hostpressure

import "errors"

// SystemSnapshot reports that host-pressure measurement is not yet available
// on this platform. Linux diagnostics are tracked separately by issue #93.
func SystemSnapshot(string) (Snapshot, error) {
	return Snapshot{}, errors.New("host-pressure measurement is only implemented on macOS")
}
