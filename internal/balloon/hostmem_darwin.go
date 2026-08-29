//go:build darwin

package balloon

import (
	"context"

	"github.com/randax/talos-box/internal/hostmem"
)

// HostTotalMiB returns physical RAM in MiB.
func HostTotalMiB() (int, error) {
	snapshot, err := hostmem.SystemSnapshot(context.Background())
	if err != nil {
		return 0, err
	}
	return snapshot.TotalMiB, nil
}

// HostFreeMiB estimates memory available to the host (free + inactive +
// speculative pages), parsed from vm_stat. This is what tbxd watches for
// pressure — a low value triggers balloon inflation.
func HostFreeMiB() (int, error) {
	return HostFreeMiBContext(context.Background())
}

// HostFreeMiBContext is HostFreeMiB bounded by ctx. Callers on a foreground
// path (doctor, cluster create) pass a deadline: vm_stat is a subprocess and a
// stalled one would otherwise hang the command with no exit code.
func HostFreeMiBContext(ctx context.Context) (int, error) {
	snapshot, err := hostmem.SystemSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	return snapshot.AvailableMiB, nil
}
