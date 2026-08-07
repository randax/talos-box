// Package hostpressure detects host resource exhaustion that can make guest
// writes unsafe even when the requested VM memory is not overcommitted.
package hostpressure

import "fmt"

const (
	extremeSwapUsedPercent       = 90
	extremeDataVolumeUsedPercent = 95
	bytesPerGiB                  = 1 << 30
)

// Usage describes capacity and immediately available bytes for one resource.
type Usage struct {
	TotalBytes     uint64
	AvailableBytes uint64
}

// Snapshot is the host capacity relevant to safe VM startup.
type Snapshot struct {
	Swap       Usage
	DataVolume Usage
}

// Warnings returns actionable diagnostics for resource usage observed at
// levels associated with guest resets or corrupt writes.
func Warnings(snapshot Snapshot) []string {
	var warnings []string
	if percentUsed(snapshot.Swap) >= extremeSwapUsedPercent {
		warnings = append(warnings, fmt.Sprintf(
			"host swap is %d%% used (%.1f GiB of %.1f GiB); guest VMs may reset and corrupt Talos EPHEMERAL data; free memory or reduce the cluster size before continuing",
			percentUsed(snapshot.Swap), gib(snapshot.Swap.usedBytes()), gib(snapshot.Swap.TotalBytes),
		))
	}
	if percentUsed(snapshot.DataVolume) >= extremeDataVolumeUsedPercent {
		warnings = append(warnings, fmt.Sprintf(
			"talosbox data volume is %d%% used (%.1f GiB free); low host storage can corrupt guest writes after a reset; free disk space before continuing",
			percentUsed(snapshot.DataVolume), gib(snapshot.DataVolume.AvailableBytes),
		))
	}
	return warnings
}

func (u Usage) usedBytes() uint64 {
	if u.AvailableBytes >= u.TotalBytes {
		return 0
	}
	return u.TotalBytes - u.AvailableBytes
}

func percentUsed(usage Usage) int {
	if usage.TotalBytes == 0 {
		return 0
	}
	return int(float64(usage.usedBytes()) * 100 / float64(usage.TotalBytes))
}

func gib(bytes uint64) float64 {
	return float64(bytes) / bytesPerGiB
}
