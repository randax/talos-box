//go:build !darwin

package hostmem

import "context"

func systemSnapshot(context.Context) (Snapshot, error) {
	return Snapshot{}, ErrUnsupported
}
