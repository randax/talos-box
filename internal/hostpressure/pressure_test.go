package hostpressure

import (
	"strings"
	"testing"
)

func TestAssessDescribesExtremeHostPressure(t *testing.T) {
	snapshot := Snapshot{
		Swap:       Usage{TotalBytes: 22_500 << 20, AvailableBytes: 1_500 << 20},
		DataVolume: Usage{TotalBytes: 1_000 << 30, AvailableBytes: 30 << 30},
	}

	findings := Assess(snapshot, 4096)
	if len(findings) != 2 {
		t.Fatalf("Assess() = %v, want swap and data-volume findings", findings)
	}
	joined := joinFindings(findings)
	for _, fragment := range []string{
		"swap is 93% used",
		"data volume is 97% used",
		"30.0 GiB free",
	} {
		if !strings.Contains(joined, fragment) {
			t.Errorf("Assess() = %v, missing %q", findings, fragment)
		}
	}
}

// Assess is the one classification behind both the daemon's start gate and
// tbx doctor, so severity is the contract that keeps them in agreement:
// SeverityBlock is exactly what create refuses on.
func TestAssessSeverityTracksMemoryPressure(t *testing.T) {
	const requiredFreeMiB = 4096
	stickySwap := Usage{TotalBytes: 18 << 30, AvailableBytes: 1 << 30} // 94% used

	tests := []struct {
		name         string
		snapshot     Snapshot
		wantSeverity Severity
		want         []string
	}{
		{
			name:     "sticky swap with normal pressure warns without blocking",
			snapshot: Snapshot{Swap: stickySwap, MemoryPressure: MemoryPressureNormal},
			// nothing is blocking, but 94% swap is worth reporting: doctor
			// must not call this a PASS while it is one pressure tick away
			// from refusing a create.
			wantSeverity: SeverityWarn,
			want:         []string{"swap is 94% used", "memory pressure is normal"},
		},
		{
			name: "full swap with elevated pressure and enough free memory warns",
			snapshot: Snapshot{
				Swap: stickySwap, MemoryPressure: MemoryPressureWarning,
				FreeMemoryMiB: 12 << 10,
			},
			wantSeverity: SeverityWarn,
			want:         []string{"swap is 94% used", "memory pressure is warning", "12288 MiB free memory", "4096 MiB required"},
		},
		{
			name: "full swap with elevated pressure and low free memory blocks",
			snapshot: Snapshot{
				Swap: stickySwap, MemoryPressure: MemoryPressureWarning,
				FreeMemoryMiB: 1 << 10,
			},
			wantSeverity: SeverityBlock,
			want:         []string{"swap is 94% used", "memory pressure is warning", "1024 MiB free memory", "4096 MiB required"},
		},
		{
			name: "full swap with unknown pressure and enough free memory warns",
			snapshot: Snapshot{
				Swap: stickySwap, FreeMemoryMiB: 12 << 10,
			},
			wantSeverity: SeverityWarn,
			want:         []string{"swap is 94% used", "memory pressure could not be measured", "12288 MiB free memory", "4096 MiB required"},
		},
		{
			name:         "full swap with unknown pressure and unmeasured free memory blocks",
			snapshot:     Snapshot{Swap: stickySwap},
			wantSeverity: SeverityBlock,
			want:         []string{"swap is 94% used", "memory pressure could not be measured"},
		},
		{
			name:         "critical pressure blocks despite abundant free memory",
			snapshot:     Snapshot{MemoryPressure: MemoryPressureCritical, FreeMemoryMiB: 12 << 10},
			wantSeverity: SeverityBlock,
			want:         []string{"memory pressure is critical"},
		},
		{
			name:     "normal pressure without swap trouble is quiet",
			snapshot: Snapshot{MemoryPressure: MemoryPressureNormal},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			findings := Assess(test.snapshot, requiredFreeMiB)
			if len(test.want) == 0 {
				if len(findings) != 0 {
					t.Fatalf("Assess() = %v, want none", findings)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("Assess() = %v, want exactly one finding", findings)
			}
			if findings[0].Severity != test.wantSeverity {
				t.Errorf("Assess() severity = %v, want %v", findings[0].Severity, test.wantSeverity)
			}
			for _, fragment := range test.want {
				if !strings.Contains(findings[0].String(), fragment) {
					t.Errorf("Assess() = %v, missing %q", findings, fragment)
				}
			}
		})
	}
}

// Swap severity must be monotonic within a pressure level: doctor reported
// PASS at 96% swap and WARN at 91% because the two consumers used different
// inputs (#284).
func TestAssessSwapSeverityIsMonotonicWithinAPressureLevel(t *testing.T) {
	for _, pressure := range []MemoryPressure{
		MemoryPressureUnknown, MemoryPressureNormal, MemoryPressureWarning, MemoryPressureCritical,
	} {
		worse := Assess(Snapshot{
			Swap:           Usage{TotalBytes: 100 << 30, AvailableBytes: 4 << 30}, // 96%
			MemoryPressure: pressure,
		}, 4096)
		better := Assess(Snapshot{
			Swap:           Usage{TotalBytes: 100 << 30, AvailableBytes: 9 << 30}, // 91%
			MemoryPressure: pressure,
		}, 4096)
		if maxSeverity(worse) < maxSeverity(better) {
			t.Errorf("pressure %v: 96%% swap severity %v is milder than 91%% swap severity %v",
				pressure, maxSeverity(worse), maxSeverity(better))
		}
	}
}

// Elevated memory pressure is the condition that corrupted guest disks, so it
// is reportable on its own: doctor PASSed at 87-88% swap while pressure was
// already `warning` because only the 90% swap threshold was consulted (#284).
func TestAssessReportsElevatedMemoryPressureBelowTheSwapThreshold(t *testing.T) {
	snapshot := Snapshot{
		// 50% used, 5 GiB free: no swap trouble by either swap rule
		Swap:           Usage{TotalBytes: 10 << 30, AvailableBytes: 5 << 30},
		MemoryPressure: MemoryPressureWarning,
	}

	findings := Assess(snapshot, 4096)
	if len(findings) != 1 {
		t.Fatalf("Assess() = %v, want a memory-pressure finding", findings)
	}
	if findings[0].Severity != SeverityWarn {
		t.Errorf("Assess() severity = %v, want SeverityWarn for warning pressure with swap to spare", findings[0].Severity)
	}
	for _, fragment := range []string{"memory pressure is warning", "swap is 50% used", "5.0 GiB free", "`tbx "} {
		if !strings.Contains(findings[0].String(), fragment) {
			t.Errorf("Assess() = %q, missing %q", findings[0], fragment)
		}
	}
}

// Free swap, not percent-used, is what a guest write needs: a host with 1 GiB
// left is out of headroom whether that is 88% or 95% of the swap file.
func TestAssessBlocksOnLowFreeSwapWithElevatedPressure(t *testing.T) {
	lowFree := Usage{TotalBytes: 20 << 30, AvailableBytes: 1 << 30} // 95% used but, more to the point, 1 GiB free
	roomy := Usage{TotalBytes: 20 << 30, AvailableBytes: 4 << 30}

	tests := []struct {
		name         string
		snapshot     Snapshot
		wantSeverity Severity
	}{
		{
			// 88% used — under the 90% swapExhausted rule, so only the
			// low-free-swap escalation can produce this Block.
			name: "low free swap with warning pressure blocks",
			snapshot: Snapshot{
				Swap:           Usage{TotalBytes: 12 << 30, AvailableBytes: 12 << 30 * 12 / 100},
				MemoryPressure: MemoryPressureWarning, FreeMemoryMiB: 1024,
			},
			wantSeverity: SeverityBlock,
		},
		{
			// A small, mostly-free swap allocation is the normal macOS reading
			// when a host merely begins to swap: warn (for the pressure), but
			// never block on the low absolute free bytes alone.
			name:         "small mostly-free swap with warning pressure only warns",
			snapshot:     Snapshot{Swap: Usage{TotalBytes: 1 << 30, AvailableBytes: 824 << 20}, MemoryPressure: MemoryPressureWarning},
			wantSeverity: SeverityWarn,
		},
		{
			name:         "low free swap with normal pressure only warns",
			snapshot:     Snapshot{Swap: lowFree, MemoryPressure: MemoryPressureNormal},
			wantSeverity: SeverityWarn,
		},
		{
			name:         "roomy swap with warning pressure stays a warning",
			snapshot:     Snapshot{Swap: roomy, MemoryPressure: MemoryPressureWarning},
			wantSeverity: SeverityWarn,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			findings := Assess(test.snapshot, 4096)
			if len(findings) != 1 {
				t.Fatalf("Assess() = %v, want exactly one memory finding", findings)
			}
			if findings[0].Severity != test.wantSeverity {
				t.Fatalf("Assess() = %v (severity %v), want severity %v", findings[0], findings[0].Severity, test.wantSeverity)
			}
		})
	}
}

// The readings QA observed while guest disks were being corrupted must never
// classify as "nothing to report" again (#284). Both are taken verbatim from
// the runs that produced the damage.
func TestAssessFlagsTheQAObservedReadings(t *testing.T) {
	tests := []struct {
		name     string
		snapshot Snapshot
	}{
		{
			// #288 QA run: doctor PASSed host-pressure at 87% swap used while
			// the kernel already reported warning memory pressure.
			name: "qa 288 run: 87% swap used with warning pressure",
			snapshot: Snapshot{
				Swap:           Usage{TotalBytes: 11 * bytesPerGiB, AvailableBytes: 11 * bytesPerGiB * 13 / 100},
				MemoryPressure: MemoryPressureWarning,
				FreeMemoryMiB:  1024,
			},
		},
		{
			// deep-storage QA run (#293): swap 8.75 GiB of 10 GiB with warning
			// pressure, during which a worker kubelet crashlooped on SIGSEGV.
			name: "deep-storage run: 8.75 GiB of 10 GiB swap with warning pressure",
			snapshot: Snapshot{
				Swap:           Usage{TotalBytes: 10 * bytesPerGiB, AvailableBytes: 10*bytesPerGiB - 8_750*bytesPerGiB/1000},
				MemoryPressure: MemoryPressureWarning,
				FreeMemoryMiB:  1024,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			findings := Assess(test.snapshot, 4096)
			if len(findings) == 0 {
				t.Fatal("Assess() = none, want the reading reported rather than a silent PASS")
			}
			// Both readings preceded observed guest corruption, and both sit
			// under 90% with <1.5 GiB free: only the low-free-swap escalation
			// makes them Block, so pinning Block pins that rule.
			if maxSeverity(findings) != SeverityBlock {
				t.Fatalf("Assess() = %v, want SeverityBlock", findings)
			}
			joined := joinFindings(findings)
			if !strings.Contains(joined, "memory pressure is warning") {
				t.Errorf("Assess() = %q, want the measured pressure named", joined)
			}
			if !strings.Contains(joined, "swap is 87% used") {
				t.Errorf("Assess() = %q, want the measured swap percentage named", joined)
			}
		})
	}
}

func TestAssessFindingsCarryRunnableRemedies(t *testing.T) {
	findings := Assess(Snapshot{
		Swap:       Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
		DataVolume: Usage{TotalBytes: 100 << 30, AvailableBytes: 5 << 30},
	}, 4096)
	if len(findings) != 2 {
		t.Fatalf("Assess() = %v, want swap and data-volume findings", findings)
	}
	for _, finding := range findings {
		if !strings.Contains(finding.Remedy, "`tbx ") {
			t.Errorf("Finding.Remedy = %q, want a runnable tbx command", finding.Remedy)
		}
		detail := finding.DoctorDetail()
		if !strings.Contains(detail, finding.Remedy) {
			t.Errorf("Finding.DoctorDetail() = %q, missing remedy %q", detail, finding.Remedy)
		}
		if finding.Severity == SeverityBlock && !strings.Contains(detail, "--force") {
			t.Errorf("Finding.DoctorDetail() = %q, want --force semantics for a blocking finding", detail)
		}
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

func TestAssessUsesExtremeThresholds(t *testing.T) {
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
			if got := len(Assess(test.snapshot, 4096)); got != test.want {
				t.Fatalf("len(Assess()) = %d, want %d", got, test.want)
			}
		})
	}
}

func joinFindings(findings []Finding) string {
	rendered := make([]string, 0, len(findings))
	for _, finding := range findings {
		rendered = append(rendered, finding.String())
	}
	return strings.Join(rendered, "\n")
}

func maxSeverity(findings []Finding) Severity {
	var highest Severity
	for _, finding := range findings {
		if finding.Severity > highest {
			highest = finding.Severity
		}
	}
	return highest
}
