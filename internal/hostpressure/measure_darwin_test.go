//go:build darwin

package hostpressure

import (
	"context"
	"testing"

	"github.com/randax/talos-box/internal/hostmem"
)

func TestSystemMemorySnapshotUsesSharedSample(t *testing.T) {
	original := hostmem.SystemSnapshot
	t.Cleanup(func() { hostmem.SystemSnapshot = original })
	hostmem.SystemSnapshot = func(context.Context) (hostmem.Snapshot, error) {
		return hostmem.Snapshot{TotalMiB: 32768, AvailableMiB: 6131, CompressorMiB: 8419,
			SwapTotalBytes: 3 << 30, SwapAvailableBytes: 410 << 20, Pressure: hostmem.PressureWarning}, nil
	}
	snapshot, err := SystemMemorySnapshotContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TotalMemoryMiB != 32768 || snapshot.FreeMemoryMiB != 6131 || snapshot.CompressorMiB != 8419 ||
		snapshot.Swap.TotalBytes != 3<<30 || snapshot.MemoryPressure != MemoryPressureWarning {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}
