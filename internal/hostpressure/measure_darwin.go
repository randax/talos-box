//go:build darwin

package hostpressure

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/randax/talos-box/internal/hostmem"
	"golang.org/x/sys/unix"
)

// sysctlTimeout bounds each probe subprocess; system utilities can stall
// behind stuck directory services or security agents, and a hung probe would
// hang cluster create/start and doctor with it.
const sysctlTimeout = 10 * time.Second

// SystemSnapshot measures macOS swap and the volume that contains path.
func SystemSnapshot(path string) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sysctlTimeout)
	defer cancel()
	memory, err := SystemMemorySnapshotContext(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	dataVolume, err := measureDataVolume(path)
	if err != nil {
		return Snapshot{}, err
	}
	memory.DataVolume = dataVolume
	return memory, nil
}

func SystemMemorySnapshot() (Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sysctlTimeout)
	defer cancel()
	return SystemMemorySnapshotContext(ctx)
}

func SystemMemorySnapshotContext(ctx context.Context) (Snapshot, error) {
	sample, err := hostmem.SystemSnapshot(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	pressure := MemoryPressureUnknown
	switch sample.Pressure {
	case hostmem.PressureNormal:
		pressure = MemoryPressureNormal
	case hostmem.PressureWarning:
		pressure = MemoryPressureWarning
	case hostmem.PressureCritical:
		pressure = MemoryPressureCritical
	}
	return Snapshot{
		Swap:           Usage{TotalBytes: sample.SwapTotalBytes, AvailableBytes: sample.SwapAvailableBytes},
		MemoryPressure: pressure, FreeMemoryMiB: sample.AvailableMiB,
		TotalMemoryMiB: sample.TotalMiB, CompressorMiB: sample.CompressorMiB,
	}, nil
}

func measureDataVolume(path string) (Usage, error) {
	path = filepath.Clean(path)
	for {
		var stats unix.Statfs_t
		err := unix.Statfs(path, &stats)
		if err == nil {
			blockSize := uint64(stats.Bsize)
			return Usage{
				TotalBytes:     stats.Blocks * blockSize,
				AvailableBytes: stats.Bavail * blockSize,
			}, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return Usage{}, fmt.Errorf("read data volume usage for %s: %w", path, err)
		}
		path = parent
	}
}
