//go:build darwin

package hostpressure

import "testing"

func TestParseSwapUsageReadsMacOSSysctlOutput(t *testing.T) {
	usage, err := parseSwapUsage("total = 22500.00M  used = 21000.00M  free = 1500.00M  (encrypted)\n")
	if err != nil {
		t.Fatal(err)
	}
	if usage.TotalBytes != 22_500<<20 || usage.AvailableBytes != 1_500<<20 {
		t.Fatalf("parseSwapUsage() = %+v, want 22500 MiB total and 1500 MiB available", usage)
	}
}
