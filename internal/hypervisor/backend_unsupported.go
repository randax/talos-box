//go:build (!darwin || !arm64) && !linux

package hypervisor

import (
	"context"
	"fmt"
	"runtime"
)

// NewAll reports the unsupported platform as an unavailable compiled default.
func NewAll(ctx context.Context) Registry {
	selection := Default{Name: Name("unsupported"), Source: DefaultSourceCompiled}
	return newRegistry(ctx, selection, []backendFactory{{
		name: selection.Name,
		new: func(context.Context) (Hypervisor, error) {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("probe hypervisor: %w", err)
			}
			return nil, fmt.Errorf("%w: no hypervisor backend for %s", ErrUnsupported, runtime.GOOS)
		},
	}})
}
