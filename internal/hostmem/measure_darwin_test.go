//go:build darwin

package hostmem

import (
	"context"
	"testing"
)

func TestSnapshotFromDarwinOutputs(t *testing.T) {
	tests := []struct {
		name          string
		vmstat        string
		swap          string
		totalBytes    uint64
		pressureLevel string
		availableMiB  int
		compressorMiB int
		swapTotal     uint64
		swapFree      uint64
	}{
		{
			name: "4 KiB pages",
			vmstat: `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages free:                               1000.
Pages inactive:                           500.
Pages speculative:                        100.
Pages occupied by compressor:             250.
`,
			swap:       "total = 3072.00M  used = 2662.40M  free = 409.60M",
			totalBytes: 32 << 30, pressureLevel: "2", availableMiB: 6, compressorMiB: 0,
			swapTotal: 3 << 30, swapFree: 429496729,
		},
		{
			name: "16 KiB pages and missing compressor",
			vmstat: `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                256.
Pages inactive:                            128.
Pages speculative:                         64.
`,
			swap:       "total = 0.00M  used = 0.00M  free = 0.00M",
			totalBytes: 16 << 30, pressureLevel: "1", availableMiB: 7, compressorMiB: 0,
		},
		{
			name: "incident fixture",
			vmstat: `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages free:                              5617.
Pages inactive:                       1560000.
Pages speculative:                       4096.
Pages occupied by compressor:         2155414.
`,
			swap:       "total = 3072.00M  used = 2662.40M  free = 409.60M",
			totalBytes: 32 << 30, pressureLevel: "2", availableMiB: 6131, compressorMiB: 8419,
			swapTotal: 3 << 30, swapFree: 429496729,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := snapshotFromDarwinOutputs(test.vmstat, test.swap, test.totalBytes, test.pressureLevel)
			if err != nil {
				t.Fatal(err)
			}
			if got.AvailableMiB != test.availableMiB || got.CompressorMiB != test.compressorMiB ||
				got.SwapTotalBytes != test.swapTotal || got.SwapAvailableBytes != test.swapFree {
				t.Fatalf("snapshot = %+v", got)
			}
		})
	}
}

func TestSnapshotFromDarwinOutputsRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name   string
		vmstat string
		swap   string
	}{
		{"no counters", "Mach Virtual Memory Statistics: (page size of 4096 bytes)\n", "total = 0.00M used = 0.00M free = 0.00M"},
		{"malformed counter", "Pages free: nope.\n", "total = 0.00M used = 0.00M free = 0.00M"},
		{"malformed swap", "Pages free: 1.\n", "total = nope free = 0.00M"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := snapshotFromDarwinOutputs(test.vmstat, test.swap, 8<<30, "1"); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestTotalMiBContextReadsOnlyHwMemsize(t *testing.T) {
	original := darwinCommandOutput
	t.Cleanup(func() { darwinCommandOutput = original })
	calls := 0
	darwinCommandOutput = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		if calls != 1 {
			t.Fatalf("unexpected extra host probe call %d", calls)
		}
		return []byte("34359738368\n"), nil
	}

	total, err := TotalMiBContext(context.Background())
	if err != nil {
		t.Fatalf("TotalMiBContext() error = %v", err)
	}
	if total != 32768 {
		t.Fatalf("TotalMiBContext() = %d, want 32768", total)
	}
}

func TestAvailableSnapshotContextToleratesSwapAndPressureFailures(t *testing.T) {
	original := darwinCommandOutput
	t.Cleanup(func() { darwinCommandOutput = original })
	vmstat := `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages free:                               1000.
Pages inactive:                           500.
Pages speculative:                        100.
Pages occupied by compressor:             250.
`
	calls := []string{}
	darwinCommandOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name
		for _, arg := range args {
			call += " " + arg
		}
		calls = append(calls, call)
		switch {
		case name == "vm_stat":
			return []byte(vmstat), nil
		case len(args) >= 2 && args[1] == "vm.swapusage":
			return nil, context.DeadlineExceeded
		case len(args) >= 2 && args[1] == "kern.memorystatus_vm_pressure_level":
			return nil, context.DeadlineExceeded
		default:
			t.Fatalf("unexpected probe %q", call)
			return nil, nil
		}
	}

	snapshot, err := AvailableSnapshotContext(context.Background())
	if err != nil {
		t.Fatalf("AvailableSnapshotContext() error = %v", err)
	}
	if snapshot.AvailableMiB != 6 || snapshot.CompressorMiB != 0 {
		t.Fatalf("AvailableSnapshotContext() = %+v", snapshot)
	}
	if snapshot.SwapTotalBytes != 0 || snapshot.SwapAvailableBytes != 0 || snapshot.Pressure != PressureUnknown {
		t.Fatalf("AvailableSnapshotContext() did not degrade missing fields: %+v", snapshot)
	}
	if len(calls) != 3 {
		t.Fatalf("probe calls = %v, want vm_stat plus optional swap/pressure sysctls", calls)
	}
}

func TestAvailableSnapshotContextDegradesMalformedOptionalSwapOutput(t *testing.T) {
	original := darwinCommandOutput
	t.Cleanup(func() { darwinCommandOutput = original })
	vmstat := `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages free:                               1000.
Pages inactive:                           500.
Pages speculative:                        100.
Pages occupied by compressor:             250.
`
	darwinCommandOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case name == "vm_stat":
			return []byte(vmstat), nil
		case len(args) >= 2 && args[1] == "vm.swapusage":
			return []byte("garbled"), nil
		case len(args) >= 2 && args[1] == "kern.memorystatus_vm_pressure_level":
			return []byte("2\n"), nil
		default:
			t.Fatalf("unexpected probe %q %q", name, args)
			return nil, nil
		}
	}

	snapshot, err := AvailableSnapshotContext(context.Background())
	if err != nil {
		t.Fatalf("AvailableSnapshotContext() error = %v", err)
	}
	if snapshot.AvailableMiB != 6 || snapshot.CompressorMiB != 0 {
		t.Fatalf("AvailableSnapshotContext() = %+v", snapshot)
	}
	if snapshot.SwapTotalBytes != 0 || snapshot.SwapAvailableBytes != 0 {
		t.Fatalf("AvailableSnapshotContext() kept malformed swap data: %+v", snapshot)
	}
	if snapshot.Pressure != PressureWarning {
		t.Fatalf("AvailableSnapshotContext() pressure = %v, want warning", snapshot.Pressure)
	}
}

func TestSystemSnapshotStillRequiresSwapAndPressureReads(t *testing.T) {
	original := darwinCommandOutput
	t.Cleanup(func() { darwinCommandOutput = original })
	vmstat := `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages free:                               1000.
Pages inactive:                           500.
Pages speculative:                        100.
Pages occupied by compressor:             250.
`
	darwinCommandOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case name == "vm_stat":
			return []byte(vmstat), nil
		case len(args) >= 2 && args[1] == "vm.swapusage":
			return nil, context.DeadlineExceeded
		case len(args) >= 2 && args[1] == "hw.memsize":
			return []byte("34359738368\n"), nil
		default:
			return []byte("1\n"), nil
		}
	}

	if _, err := systemSnapshot(context.Background()); err == nil {
		t.Fatal("systemSnapshot() error = nil, want strict swap/pressure read failure")
	}
}
