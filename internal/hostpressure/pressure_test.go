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

	findings := Assess(snapshot)
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
			name:         "full swap with elevated pressure blocks",
			snapshot:     Snapshot{Swap: stickySwap, MemoryPressure: MemoryPressureWarning},
			wantSeverity: SeverityBlock,
			want:         []string{"swap is 94% used", "memory pressure is warning"},
		},
		{
			name:         "full swap with unknown pressure keeps the conservative block",
			snapshot:     Snapshot{Swap: stickySwap},
			wantSeverity: SeverityBlock,
			want:         []string{"swap is 94% used", "memory pressure could not be measured"},
		},
		{
			name:         "critical pressure blocks on its own",
			snapshot:     Snapshot{MemoryPressure: MemoryPressureCritical},
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
			findings := Assess(test.snapshot)
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
		})
		better := Assess(Snapshot{
			Swap:           Usage{TotalBytes: 100 << 30, AvailableBytes: 9 << 30}, // 91%
			MemoryPressure: pressure,
		})
		if maxSeverity(worse) < maxSeverity(better) {
			t.Errorf("pressure %v: 96%% swap severity %v is milder than 91%% swap severity %v",
				pressure, maxSeverity(worse), maxSeverity(better))
		}
	}
}

func TestAssessFindingsCarryRunnableRemedies(t *testing.T) {
	findings := Assess(Snapshot{
		Swap:       Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
		DataVolume: Usage{TotalBytes: 100 << 30, AvailableBytes: 5 << 30},
	})
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
			if got := len(Assess(test.snapshot)); got != test.want {
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
