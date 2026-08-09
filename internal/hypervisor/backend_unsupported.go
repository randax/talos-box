//go:build !darwin || !arm64

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
