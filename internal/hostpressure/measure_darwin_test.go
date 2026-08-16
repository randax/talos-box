//go:build darwin

package hostpressure

import "testing"

func TestMemoryPressureFromLevelMapsSysctlValues(t *testing.T) {
	for value, want := range map[string]MemoryPressure{
		"1":       MemoryPressureNormal,
		"2":       MemoryPressureWarning,
		"4":       MemoryPressureCritical,
		"0":       MemoryPressureUnknown,
		"":        MemoryPressureUnknown,
		"garbage": MemoryPressureUnknown,
	} {
		if got := memoryPressureFromLevel(value); got != want {
			t.Errorf("memoryPressureFromLevel(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestParseSwapUsageReadsMacOSSysctlOutput(t *testing.T) {
	usage, err := parseSwapUsage("total = 22500.00M  used = 21000.00M  free = 1500.00M  (encrypted)\n")
	if err != nil {
		t.Fatal(err)
	}
	if usage.TotalBytes != 22_500<<20 || usage.AvailableBytes != 1_500<<20 {
		t.Fatalf("parseSwapUsage() = %+v, want 22500 MiB total and 1500 MiB available", usage)
	}
}
