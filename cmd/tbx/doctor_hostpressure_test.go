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
		fmt.Sprintf("(measured with no guests pending: a start larger than %d MiB of new guests will still be refused)", 12243-reserve),
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

// #397/#420: TBX_BALLOON_RESERVE_MIB is read per process, so the reserve the
// gate will apply is the daemon's, not the CLI's. The PASS quotes the daemon's
// answer when there is one.
func TestRunDoctorHostPressurePassUsesDaemonReportedReserve(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.hostFreeMemory = func() (int, error) { return 12243, nil }
	deps.balloonReserveMiB = func() (int, error) { return 4096, nil }

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v for a healthy host", err)
	}

	want := fmt.Sprintf("must leave the 4096 MiB balloon reserve free, so there is room for %d MiB of new guests right now", 12243-4096)
	if !strings.Contains(output.String(), want) {
		t.Errorf("output missing %q:\n%s", want, output.String())
	}
	if strings.Contains(output.String(), "daemon unreachable") {
		t.Errorf("output claims the daemon was unreachable:\n%s", output.String())
	}
}

func TestRunDoctorHostPressureBelowBalloonReserveReportsNoCapacity(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.hostFreeMemory = func() (int, error) { return 6056, nil }
	deps.balloonReserveMiB = func() (int, error) { return 6144, nil }

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v for a healthy host below the reserve", err)
	}

	want := "the host is already 88 MiB below the balloon reserve; any new guest start will be refused until memory frees"
	if !strings.Contains(output.String(), want) {
		t.Errorf("output missing %q:\n%s", want, output.String())
	}
	if strings.Contains(output.String(), "larger than -88 MiB") {
		t.Errorf("output reports negative guest capacity:\n%s", output.String())
	}
}

// With no daemon to ask, the CLI's own default is a guess about the gate, and
// the line says so rather than presenting it as the gate's number.
func TestRunDoctorHostPressurePassSaysTheReserveWasAssumed(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.hostFreeMemory = func() (int, error) { return 12243, nil }
	deps.balloonReserveMiB = func() (int, error) { return 0, errors.New("daemon not running") }

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v for a healthy host", err)
	}

	want := fmt.Sprintf("must leave the %d MiB balloon reserve free", balloon.DefaultConfig().ReserveMiB)
	if !strings.Contains(output.String(), want) {
		t.Errorf("output missing %q:\n%s", want, output.String())
	}
	if !strings.Contains(output.String(), "(daemon unreachable; assuming the default reserve)") {
		t.Errorf("output missing the assumed-reserve caveat:\n%s", output.String())
	}
}

func TestRunDoctorClassifiesHighSwapAgainstMeasuredMemoryHeadroom(t *testing.T) {
	tests := []struct {
		name         string
		pressure     hostpressure.MemoryPressure
		freeMemory   func() (int, error)
		wantLevel    string
		wantExitFail bool
	}{
		{
			name:       "roomy stale swap warns",
			pressure:   hostpressure.MemoryPressureWarning,
			freeMemory: func() (int, error) { return 12 << 10, nil },
			wantLevel:  "WARN",
		},
		{
			name:         "low free memory fails",
			pressure:     hostpressure.MemoryPressureWarning,
			freeMemory:   func() (int, error) { return 1 << 10, nil },
			wantLevel:    "FAIL",
			wantExitFail: true,
		},
		{
			name:         "unmeasured free memory fails",
			pressure:     hostpressure.MemoryPressureWarning,
			freeMemory:   func() (int, error) { return 0, errors.New("vm_stat unavailable") },
			wantLevel:    "FAIL",
			wantExitFail: true,
		},
		{
			name:         "critical pressure fails despite abundant free memory",
			pressure:     hostpressure.MemoryPressureCritical,
			freeMemory:   func() (int, error) { return 12 << 10, nil },
			wantLevel:    "FAIL",
			wantExitFail: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := passingDoctorDependencies()
			deps.hostPressure = func() (hostpressure.Snapshot, error) {
				return hostpressure.Snapshot{
					Swap:           hostpressure.Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
					MemoryPressure: test.pressure,
				}, nil
			}
			deps.hostFreeMemory = test.freeMemory
			deps.balloonReserveMiB = func() (int, error) { return 6144, nil }

			var output strings.Builder
			err := (cli{out: &output}).runDoctorWithDependencies(nil, deps)
			if test.wantExitFail != (err != nil) {
				t.Fatalf("runDoctorWithDependencies() error = %v, want failure = %v", err, test.wantExitFail)
			}
			want := test.wantLevel + " host-pressure:"
			if !strings.Contains(output.String(), want) {
				t.Fatalf("output missing %q:\n%s", want, output.String())
			}
			caveat := "(measured with no guests pending: a start larger than 6144 MiB of new guests will still be refused)"
			if test.wantLevel == "WARN" && !strings.Contains(output.String(), caveat) {
				t.Fatalf("output missing %q:\n%s", caveat, output.String())
			}
			if test.wantLevel == "FAIL" && strings.Contains(output.String(), caveat) {
				t.Fatalf("blocking output unexpectedly contains %q:\n%s", caveat, output.String())
			}
		})
	}
}

func TestRunDoctorWarnsAtEightyPercentSwapUnderNormalPressure(t *testing.T) {
	deps := passingDoctorDependencies()
	deps.hostPressure = func() (hostpressure.Snapshot, error) {
		return hostpressure.Snapshot{
			Swap:           hostpressure.Usage{TotalBytes: 3 << 30, AvailableBytes: 3 << 30 * 13 / 100},
			MemoryPressure: hostpressure.MemoryPressureNormal,
		}, nil
	}
	deps.hostFreeMemory = func() (int, error) { return 16 << 10, nil }

	var output strings.Builder
	if err := (cli{out: &output}).runDoctorWithDependencies(nil, deps); err != nil {
		t.Fatalf("runDoctorWithDependencies() = %v, warning must not fail doctor", err)
	}
	if !strings.Contains(output.String(), "WARN host-pressure: host swap is 87% used") {
		t.Fatalf("output missing steady swap warning:\n%s", output.String())
	}
}
