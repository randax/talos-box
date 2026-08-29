//go:build darwin

package hostmem

import (
	"context"
	"os/exec"
	"testing"
	"time"
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

func TestLiveSystemSnapshot(t *testing.T) {
	if _, err := exec.LookPath("vm_stat"); err != nil {
		t.Skip("vm_stat is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := systemSnapshot(ctx)
	if err != nil {
		t.Skipf("live host-memory probe unavailable: %v", err)
	}
	if snapshot.TotalMiB <= 0 || snapshot.AvailableMiB < 0 {
		t.Fatalf("implausible live snapshot: %+v", snapshot)
	}
}
