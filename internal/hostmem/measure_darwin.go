//go:build darwin

package hostmem

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const defaultPageSize = 4096

var darwinCommandOutput = commandOutput

func TotalMiBContext(ctx context.Context) (int, error) {
	totalBytes, err := readTotalBytes(ctx)
	if err != nil {
		return 0, err
	}
	return int(totalBytes / 1024 / 1024), nil
}

func AvailableSnapshotContext(ctx context.Context) (Snapshot, error) {
	vmstat, err := darwinCommandOutput(ctx, "vm_stat")
	if err != nil {
		return Snapshot{}, fmt.Errorf("vm_stat: %w", err)
	}
	pageSize, pages, err := parseVMStat(string(vmstat))
	if err != nil {
		return Snapshot{}, err
	}

	snapshot := Snapshot{
		AvailableMiB:  int((pages["Pages free"] + pages["Pages inactive"] + pages["Pages speculative"]) * uint64(pageSize) / 1024 / 1024),
		CompressorMiB: int(pages["Pages occupied by compressor"] * uint64(pageSize) / 1024 / 1024),
		Pressure:      PressureUnknown,
	}
	if swap, err := darwinCommandOutput(ctx, "/usr/sbin/sysctl", "-n", "vm.swapusage"); err == nil {
		if swapTotal, swapFree, parseErr := parseSwapUsage(string(swap)); parseErr == nil {
			snapshot.SwapTotalBytes = swapTotal
			snapshot.SwapAvailableBytes = swapFree
		}
	}
	if pressure, err := darwinCommandOutput(ctx, "/usr/sbin/sysctl", "-n", "kern.memorystatus_vm_pressure_level"); err == nil {
		snapshot.Pressure = pressureFromLevel(string(pressure))
	}
	return snapshot, nil
}

func systemSnapshot(ctx context.Context) (Snapshot, error) {
	vmstat, err := darwinCommandOutput(ctx, "vm_stat")
	if err != nil {
		return Snapshot{}, fmt.Errorf("vm_stat: %w", err)
	}
	swap, err := darwinCommandOutput(ctx, "/usr/sbin/sysctl", "-n", "vm.swapusage")
	if err != nil {
		return Snapshot{}, fmt.Errorf("read vm.swapusage: %w", err)
	}
	totalBytes, err := readTotalBytes(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	pressure := ""
	if output, pressureErr := darwinCommandOutput(ctx, "/usr/sbin/sysctl", "-n", "kern.memorystatus_vm_pressure_level"); pressureErr == nil {
		pressure = strings.TrimSpace(string(output))
	} else if ctx.Err() != nil {
		return Snapshot{}, fmt.Errorf("read memory pressure: %w", ctx.Err())
	}
	return snapshotFromDarwinOutputs(string(vmstat), string(swap), totalBytes, pressure)
}

func readTotalBytes(ctx context.Context) (uint64, error) {
	memsize, err := darwinCommandOutput(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("read hw.memsize: %w", err)
	}
	totalBytes, err := strconv.ParseUint(strings.TrimSpace(string(memsize)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse hw.memsize: %w", err)
	}
	return totalBytes, nil
}

func commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return output, err
}

func snapshotFromDarwinOutputs(vmstat, swap string, totalBytes uint64, pressureLevel string) (Snapshot, error) {
	pageSize, pages, err := parseVMStat(vmstat)
	if err != nil {
		return Snapshot{}, err
	}
	swapTotal, swapFree, err := parseSwapUsage(swap)
	if err != nil {
		return Snapshot{}, err
	}
	availablePages := pages["Pages free"] + pages["Pages inactive"] + pages["Pages speculative"]
	return Snapshot{
		TotalMiB:           int(totalBytes / 1024 / 1024),
		AvailableMiB:       int(availablePages * uint64(pageSize) / 1024 / 1024),
		CompressorMiB:      int(pages["Pages occupied by compressor"] * uint64(pageSize) / 1024 / 1024),
		SwapTotalBytes:     swapTotal,
		SwapAvailableBytes: swapFree,
		Pressure:           pressureFromLevel(pressureLevel),
	}, nil
}

func parseVMStat(output string) (int, map[string]uint64, error) {
	pageSize := defaultPageSize
	recognized := map[string]bool{
		"Pages free": true, "Pages inactive": true, "Pages speculative": true,
		"Pages occupied by compressor": true,
	}
	pages := make(map[string]uint64, len(recognized))
	found := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "page size of") {
			fields := strings.Fields(line)
			for index, field := range fields {
				if field == "of" && index+1 < len(fields) {
					value, err := strconv.Atoi(fields[index+1])
					if err != nil || value <= 0 {
						return 0, nil, fmt.Errorf("parse vm_stat page size from %q", line)
					}
					pageSize = value
					break
				}
			}
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if !recognized[name] {
			continue
		}
		text := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(parts[1]), "."))
		value, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return 0, nil, fmt.Errorf("parse vm_stat %s value %q: %w", name, text, err)
		}
		pages[name] = value
		found++
	}
	if err := scanner.Err(); err != nil {
		return 0, nil, fmt.Errorf("scan vm_stat: %w", err)
	}
	if found == 0 {
		return 0, nil, fmt.Errorf("parse vm_stat: no recognized page counters")
	}
	return pageSize, pages, nil
}

func parseSwapUsage(output string) (uint64, uint64, error) {
	fields := strings.Fields(output)
	values := make(map[string]uint64, 3)
	for index := 0; index+2 < len(fields); index++ {
		if fields[index+1] != "=" {
			continue
		}
		value, err := parseBytes(fields[index+2])
		if err != nil {
			return 0, 0, fmt.Errorf("parse swap %s: %w", fields[index], err)
		}
		values[fields[index]] = value
		index += 2
	}
	total, totalOK := values["total"]
	free, freeOK := values["free"]
	if !totalOK || !freeOK {
		return 0, 0, fmt.Errorf("parse swap usage: expected total and free values in %q", strings.TrimSpace(output))
	}
	return total, free, nil
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

func pressureFromLevel(value string) Pressure {
	switch strings.TrimSpace(value) {
	case "1":
		return PressureNormal
	case "2":
		return PressureWarning
	case "4":
		return PressureCritical
	default:
		return PressureUnknown
	}
}
