package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/hostpressure"
)

func TestRunningStatusExists(t *testing.T) {
	if runningStatusExists(nil) || runningStatusExists([]daemon.ClusterStatus{{Name: "stopped"}}) {
		t.Fatal("stopped status classified as running")
	}
	if !runningStatusExists([]daemon.ClusterStatus{{Name: "demo", Running: true}}) {
		t.Fatal("running status was not detected")
	}
}

func TestPrintStatusPressureNotice(t *testing.T) {
	tests := []struct {
		name     string
		snapshot hostpressure.Snapshot
		err      error
		want     string
	}{
		{name: "79 percent is quiet", snapshot: swapSnapshot(79)},
		{name: "80 percent warns", snapshot: swapSnapshot(80), want: "warning: host swap is 80% used"},
		{name: "87 percent small swap warns", snapshot: hostpressure.Snapshot{Swap: hostpressure.Usage{TotalBytes: 3 << 30, AvailableBytes: 3 << 30 * 13 / 100}}, want: "warning: host swap is 87% used"},
		{name: "unsupported is quiet", err: hostpressure.ErrUnsupported},
		{name: "probe failure is quiet", err: errors.New("probe failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := printStatusPressureNotice(&output, test.snapshot, test.err); err != nil {
				t.Fatal(err)
			}
			if test.want == "" && output.Len() != 0 {
				t.Fatalf("output = %q, want quiet", output.String())
			}
			if test.want != "" && !strings.Contains(output.String(), test.want) {
				t.Fatalf("output = %q, want %q", output.String(), test.want)
			}
		})
	}
}

func swapSnapshot(percent int) hostpressure.Snapshot {
	total := uint64(100 << 30)
	return hostpressure.Snapshot{Swap: hostpressure.Usage{TotalBytes: total, AvailableBytes: total * uint64(100-percent) / 100}}
}
