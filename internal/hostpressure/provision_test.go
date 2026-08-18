package hostpressure

import (
	"strings"
	"testing"
)

func TestAssessProvisionStart(t *testing.T) {
	const reserve = 4096
	tests := []struct {
		name        string
		in          ProvisionStart
		wantBlocks  int
		wantDetails []string
	}{
		{
			name: "idle host with room admits",
			in: ProvisionStart{
				NewVMMiB: 18432, HostFreeMiB: 24576, ReserveMiB: reserve,
				Swap: Usage{TotalBytes: 8 << 30, AvailableBytes: 8 << 30},
			},
		},
		{
			name: "second concurrent create refused on headroom (#334)",
			in: ProvisionStart{
				RunningVMMiB: 18432, NewVMMiB: 18432, HostFreeMiB: 20480, ReserveMiB: reserve,
			},
			wantBlocks:  1,
			wantDetails: []string{"already running", "balloon reserve"},
		},
		{
			name: "headroom exactly at the reserve admits",
			in: ProvisionStart{
				RunningVMMiB: 6144, NewVMMiB: 6144, HostFreeMiB: 6144 + reserve, ReserveMiB: reserve,
			},
		},
		{
			name: "one MiB below the reserve refuses",
			in: ProvisionStart{
				RunningVMMiB: 6144, NewVMMiB: 6144, HostFreeMiB: 6144 + reserve - 1, ReserveMiB: reserve,
			},
			wantBlocks: 1,
		},
		{
			name: "tight host with no guests running is the balloon controller's job",
			in: ProvisionStart{
				NewVMMiB: 18432, HostFreeMiB: 18432, ReserveMiB: reserve,
			},
		},
		{
			name: "unmeasurable free memory skips the headroom rule",
			in: ProvisionStart{
				RunningVMMiB: 18432, NewVMMiB: 18432, HostFreeMiB: 0, ReserveMiB: reserve,
			},
		},
		{
			name: "operation that commits no memory skips the headroom rule",
			in: ProvisionStart{
				RunningVMMiB: 18432, NewVMMiB: 0, HostFreeMiB: 512, ReserveMiB: reserve,
			},
		},
		{
			name: "nearly full swap under degraded pressure refuses a second bringup",
			in: ProvisionStart{
				RunningVMMiB: 18432, NewVMMiB: 6144, HostFreeMiB: 24576, ReserveMiB: reserve,
				// the #334 reading: 7.3 GiB of 8 GiB used while the concurrent
				// bringup had already pushed the kernel off normal
				Swap:           Usage{TotalBytes: 8 << 30, AvailableBytes: 7 << 30 / 10},
				MemoryPressure: MemoryPressureWarning,
			},
			wantBlocks:  1,
			wantDetails: []string{"host swap is 91% used", "memory pressure warning", "already running"},
		},
		{
			// #334 records host-pressure PASS at preflight: macOS had already
			// dropped back to normal while 7.3 GiB stayed swapped out. The gate
			// must refuse on the footprint alone, or it is disarmed in the very
			// state the incident was reported from.
			name: "the #334 footprint refuses even after pressure returns to normal",
			in: ProvisionStart{
				RunningVMMiB: 18432, NewVMMiB: 6144, HostFreeMiB: 24576, ReserveMiB: reserve,
				Swap:           Usage{TotalBytes: 8 << 30, AvailableBytes: 7 << 30 / 10},
				MemoryPressure: MemoryPressureNormal,
			},
			wantBlocks:  1,
			wantDetails: []string{"host swap is 91% used", "7.3 GiB of 8.0 GiB", "sustained real pressure"},
		},
		{
			name: "a small sticky swapfile under elevated pressure still refuses",
			in: ProvisionStart{
				RunningVMMiB: 18432, NewVMMiB: 6144, HostFreeMiB: 30720, ReserveMiB: reserve,
				Swap:           Usage{TotalBytes: 2 << 30, AvailableBytes: 2 << 30 * 18 / 100},
				MemoryPressure: MemoryPressureWarning,
			},
			wantBlocks:  1,
			wantDetails: []string{"pressure is off normal"},
		},
		{
			name: "a small sticky swapfile at normal pressure admits (#231)",
			in: ProvisionStart{
				// 64 GiB Mac, 30 GiB free, a 2 GiB dynamic swapfile 82% used:
				// macOS keeps swap allocated long after the pressure clears
				RunningVMMiB: 18432, NewVMMiB: 6144, HostFreeMiB: 30720, ReserveMiB: reserve,
				Swap:           Usage{TotalBytes: 2 << 30, AvailableBytes: 2 << 30 * 18 / 100},
				MemoryPressure: MemoryPressureNormal,
			},
		},
		{
			name: "an unmeasurable pressure reading keeps the swap rule armed",
			in: ProvisionStart{
				RunningVMMiB: 6144, NewVMMiB: 6144, HostFreeMiB: 24576, ReserveMiB: reserve,
				Swap: Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
			},
			wantBlocks:  1,
			wantDetails: []string{"memory pressure unmeasurable"},
		},
		{
			name: "sticky swap on an idle host is not this gate's business (#231)",
			in: ProvisionStart{
				NewVMMiB: 6144, HostFreeMiB: 24576, ReserveMiB: reserve,
				Swap:           Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
				MemoryPressure: MemoryPressureWarning,
			},
		},
		{
			name: "swap exactly at the ceiling refuses",
			in: ProvisionStart{
				RunningVMMiB: 6144, NewVMMiB: 6144, HostFreeMiB: 24576, ReserveMiB: reserve,
				Swap:           Usage{TotalBytes: 10 << 30, AvailableBytes: 2 << 30},
				MemoryPressure: MemoryPressureCritical,
			},
			wantBlocks: 1,
		},
		{
			name: "swap just under the ceiling admits",
			in: ProvisionStart{
				RunningVMMiB: 6144, NewVMMiB: 6144, HostFreeMiB: 24576, ReserveMiB: reserve,
				Swap:           Usage{TotalBytes: 100 << 30, AvailableBytes: 21 << 30},
				MemoryPressure: MemoryPressureWarning,
			},
		},
		{
			name: "swap disabled skips the swap rule",
			in: ProvisionStart{
				RunningVMMiB: 6144, NewVMMiB: 6144, HostFreeMiB: 24576, ReserveMiB: reserve,
			},
		},
		{
			name: "both rules report separately",
			in: ProvisionStart{
				RunningVMMiB: 6144, NewVMMiB: 6144, HostFreeMiB: 6144, ReserveMiB: reserve,
				Swap:           Usage{TotalBytes: 8 << 30, AvailableBytes: 1 << 30},
				MemoryPressure: MemoryPressureWarning,
			},
			wantBlocks: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			findings := AssessProvisionStart(test.in)
			blocks := 0
			joined := ""
			for _, finding := range findings {
				if finding.Severity == SeverityBlock {
					blocks++
				}
				joined += finding.String() + "\n"
			}
			if blocks != test.wantBlocks {
				t.Fatalf("AssessProvisionStart() blocking findings = %d, want %d (%q)", blocks, test.wantBlocks, joined)
			}
			for _, want := range test.wantDetails {
				if !strings.Contains(joined, want) {
					t.Errorf("AssessProvisionStart() detail %q missing from %q", want, joined)
				}
			}
			for _, finding := range findings {
				if finding.Remedy == "" {
					t.Error("AssessProvisionStart() finding has no remedy")
				}
			}
		})
	}
}

// TestAssessProvisionStartSwapCeilingIsNotSteadyState guards the #231 boundary:
// the lower swap ceiling belongs to bringup only, so a steady-state host at the
// same reading must stay a PASS for Assess.
func TestAssessProvisionStartSwapCeilingIsNotSteadyState(t *testing.T) {
	// A 2 GiB dynamic swapfile 80% used: past the bringup percentage ceiling,
	// but only 1.6 GiB actually swapped out, which is the sticky-file reading
	// #231 removed from the steady-state verdict.
	snapshot := Snapshot{
		Swap:           Usage{TotalBytes: 2 << 30, AvailableBytes: 2 << 30 * 20 / 100},
		MemoryPressure: MemoryPressureNormal,
	}
	for _, finding := range Assess(snapshot) {
		if finding.Severity == SeverityBlock {
			t.Fatalf("Assess() blocked at 80%% swap use with normal pressure: %s", finding)
		}
	}
	// The bringup gate agrees with Assess while the kernel reports normal
	// pressure and the swapped-out footprint is small — a sticky file alone is
	// not a verdict on either path — and parts company with it as soon as
	// pressure is anything else.
	admitted := AssessProvisionStart(ProvisionStart{
		RunningVMMiB: 6144, NewVMMiB: 6144, HostFreeMiB: 1 << 20, ReserveMiB: 4096,
		Swap: snapshot.Swap, MemoryPressure: snapshot.MemoryPressure,
	})
	if len(admitted) != 0 {
		t.Fatalf("AssessProvisionStart() = %v, want no finding at 80%% swap use with normal pressure", admitted)
	}
	got := AssessProvisionStart(ProvisionStart{
		RunningVMMiB: 6144, NewVMMiB: 6144, HostFreeMiB: 1 << 20, ReserveMiB: 4096,
		Swap: snapshot.Swap, MemoryPressure: MemoryPressureWarning,
	})
	if len(got) != 1 || got[0].Severity != SeverityBlock {
		t.Fatalf("AssessProvisionStart() = %v, want one blocking swap finding", got)
	}
}
