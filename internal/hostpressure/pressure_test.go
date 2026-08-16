package hostpressure

import (
	"strings"
	"testing"
)

func TestWarningsDescribeExtremeHostPressure(t *testing.T) {
	snapshot := Snapshot{
		Swap:       Usage{TotalBytes: 22_500 << 20, AvailableBytes: 1_500 << 20},
		DataVolume: Usage{TotalBytes: 1_000 << 30, AvailableBytes: 30 << 30},
	}

	warnings := Warnings(snapshot)
	if len(warnings) != 2 {
		t.Fatalf("Warnings() = %v, want swap and data-volume warnings", warnings)
	}
	for _, fragment := range []string{
		"swap is 93% used",
		"free memory or reduce the cluster size",
		"data volume is 97% used",
		"30.0 GiB free",
		"free disk space",
	} {
		if !strings.Contains(strings.Join(warnings, "\n"), fragment) {
			t.Errorf("Warnings() = %v, missing %q", warnings, fragment)
		}
	}
}

func TestWarningsTreatSwapAsSecondaryToMemoryPressure(t *testing.T) {
	stickySwap := Usage{TotalBytes: 18 << 30, AvailableBytes: 1 << 30} // 94% used

	tests := []struct {
		name     string
		snapshot Snapshot
		want     []string
	}{
		{
			name:     "sticky swap with normal pressure is fine",
			snapshot: Snapshot{Swap: stickySwap, MemoryPressure: MemoryPressureNormal},
			want:     nil,
		},
		{
			name:     "full swap with elevated pressure warns",
			snapshot: Snapshot{Swap: stickySwap, MemoryPressure: MemoryPressureWarning},
			want:     []string{"swap is 94% used", "memory pressure is warning"},
		},
		{
			name:     "full swap with unknown pressure keeps the conservative warning",
			snapshot: Snapshot{Swap: stickySwap},
			want:     []string{"swap is 94% used", "memory pressure could not be measured"},
		},
		{
			name:     "critical pressure warns on its own",
			snapshot: Snapshot{MemoryPressure: MemoryPressureCritical},
			want:     []string{"memory pressure is critical", "free memory or reduce the cluster size"},
		},
		{
			name:     "normal pressure without swap trouble is quiet",
			snapshot: Snapshot{MemoryPressure: MemoryPressureNormal},
			want:     nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			warnings := Warnings(test.snapshot)
			if len(test.want) == 0 {
				if len(warnings) != 0 {
					t.Fatalf("Warnings() = %v, want none", warnings)
				}
				return
			}
			joined := strings.Join(warnings, "\n")
			for _, fragment := range test.want {
				if !strings.Contains(joined, fragment) {
					t.Errorf("Warnings() = %v, missing %q", warnings, fragment)
				}
			}
		})
	}
}

func TestMemoryPressureString(t *testing.T) {
	for pressure, want := range map[MemoryPressure]string{
		MemoryPressureUnknown:  "unknown",
		MemoryPressureNormal:   "normal",
		MemoryPressureWarning:  "warning",
		MemoryPressureCritical: "critical",
	} {
		if got := pressure.String(); got != want {
			t.Errorf("MemoryPressure(%d).String() = %q, want %q", pressure, got, want)
		}
	}
}

func TestWarningsUseExtremeThresholds(t *testing.T) {
	tests := []struct {
		name     string
		snapshot Snapshot
		want     int
	}{
		{name: "swap below threshold", snapshot: Snapshot{Swap: Usage{TotalBytes: 100, AvailableBytes: 11}}},
		{name: "swap at threshold", snapshot: Snapshot{Swap: Usage{TotalBytes: 100, AvailableBytes: 10}}, want: 1},
		{name: "data volume below threshold", snapshot: Snapshot{DataVolume: Usage{TotalBytes: 100, AvailableBytes: 6}}},
		{name: "data volume at threshold", snapshot: Snapshot{DataVolume: Usage{TotalBytes: 100, AvailableBytes: 5}}, want: 1},
		{name: "swap disabled", snapshot: Snapshot{Swap: Usage{}}, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := len(Warnings(test.snapshot)); got != test.want {
				t.Fatalf("len(Warnings()) = %d, want %d", got, test.want)
			}
		})
	}
}
