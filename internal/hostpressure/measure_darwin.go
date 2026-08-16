//go:build darwin

package hostpressure

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// SystemSnapshot measures macOS swap and the volume that contains path.
func SystemSnapshot(path string) (Snapshot, error) {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "vm.swapusage").Output()
	if err != nil {
		return Snapshot{}, fmt.Errorf("read swap usage: %w", err)
	}
	swap, err := parseSwapUsage(string(out))
	if err != nil {
		return Snapshot{}, err
	}
	dataVolume, err := measureDataVolume(path)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Swap: swap, DataVolume: dataVolume, MemoryPressure: measureMemoryPressure()}, nil
}

// measureMemoryPressure reads the kernel's pressure verdict; failures degrade
// to Unknown rather than failing the snapshot, which keeps the conservative
// swap warning in force.
func measureMemoryPressure() MemoryPressure {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "kern.memorystatus_vm_pressure_level").Output()
	if err != nil {
		return MemoryPressureUnknown
	}
	return memoryPressureFromLevel(strings.TrimSpace(string(out)))
}

func memoryPressureFromLevel(value string) MemoryPressure {
	switch value {
	case "1":
		return MemoryPressureNormal
	case "2":
		return MemoryPressureWarning
	case "4":
		return MemoryPressureCritical
	default:
		return MemoryPressureUnknown
	}
}

func parseSwapUsage(output string) (Usage, error) {
	fields := strings.Fields(output)
	values := make(map[string]uint64, 3)
	for index := 0; index+2 < len(fields); index++ {
		if fields[index+1] != "=" {
			continue
		}
		value, err := parseBytes(fields[index+2])
		if err != nil {
			return Usage{}, fmt.Errorf("parse swap %s: %w", fields[index], err)
		}
		values[fields[index]] = value
		index += 2
	}
	total, hasTotal := values["total"]
	available, hasFree := values["free"]
	if !hasTotal || !hasFree {
		return Usage{}, fmt.Errorf("parse swap usage: expected total and free values in %q", strings.TrimSpace(output))
	}
	return Usage{TotalBytes: total, AvailableBytes: available}, nil
}

func parseBytes(value string) (uint64, error) {
	if len(value) < 2 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	multiplier := float64(1)
	switch value[len(value)-1] {
	case 'K':
		multiplier = 1 << 10
	case 'M':
		multiplier = 1 << 20
	case 'G':
		multiplier = 1 << 30
	case 'T':
		multiplier = 1 << 40
	default:
		return 0, fmt.Errorf("unknown size unit in %q", value)
	}
	number, err := strconv.ParseFloat(value[:len(value)-1], 64)
	if err != nil || number < 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	return uint64(number * multiplier), nil
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
