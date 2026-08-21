//go:build darwin

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/hostpressure"
)

// #420: a PASS with no numbers cannot be attested — several runbooks require
// quoting the measured readings — and #397: a host can read PASS here and still
// be refused its second cluster, so the PASS carries the gate's own arithmetic.
func TestRunDoctorHostPressurePassPrintsMeasuredNumbers(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.hostPressure = func() (hostpressure.Snapshot, error) {
		return hostpressure.Snapshot{
			Swap:           hostpressure.Usage{TotalBytes: 8 << 30, AvailableBytes: 6 << 30},
			DataVolume:     hostpressure.Usage{TotalBytes: 1000 << 30, AvailableBytes: 400 << 30},
			MemoryPressure: hostpressure.MemoryPressureNormal,
		}, nil
	}
	deps.hostFreeMemory = func() (int, error) { return 12243, nil }

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v for a healthy host", err)
	}

	reserve := balloon.DefaultConfig().ReserveMiB
	for _, fragment := range []string{
		"PASS host-pressure: 12243 MiB free memory",
		"2.0 GiB of 8.0 GiB swap in use",
		"400.0 GiB free on the ~/.talosbox volume",
		fmt.Sprintf("must leave the %d MiB balloon reserve free, so there is room for %d MiB of new guests right now", reserve, 12243-reserve),
	} {
		if !strings.Contains(output.String(), fragment) {
			t.Errorf("output missing %q:\n%s", fragment, output.String())
		}
	}
}

// An unreadable free-memory probe still reports the two numbers it has, and
// says so, rather than failing a check the snapshot itself passed.
func TestRunDoctorHostPressurePassWithoutFreeMemoryReading(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.hostFreeMemory = func() (int, error) { return 0, errors.New("vm_stat: not found") }

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v with an unreadable free-memory probe", err)
	}
	if !strings.Contains(output.String(), "PASS host-pressure: free memory unmeasured") {
		t.Errorf("output missing the unmeasured free-memory summary:\n%s", output.String())
	}
}
