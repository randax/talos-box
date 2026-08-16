//go:build (!darwin || !arm64) && !linux

package hypervisor

import (
	"context"
	"fmt"
	"runtime"
)

// New reports that no backend is available for the current platform.
func New(ctx context.Context) (Hypervisor, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("probe hypervisor: %w", err)
	}
	return nil, fmt.Errorf("%w: no hypervisor backend for %s", ErrUnsupported, runtime.GOOS)
}

// GuestAgentSupport reports the host's guest-agent capability without probing a
// backend, so `tbx doctor` can explain the gate with the daemon down.
func GuestAgentSupport() FeatureStatus {
	return FeatureStatus{Reason: "no hypervisor backend for " + runtime.GOOS}
}
