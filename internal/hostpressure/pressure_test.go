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
