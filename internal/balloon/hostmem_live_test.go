//go:build darwin && live

// Run live host-memory probes with:
// go test -tags live ./internal/hostmem ./internal/balloon
package balloon

import "testing"

func TestHostMemReadsSaneValues(t *testing.T) {
	total, err := HostTotalMiB()
	if err != nil {
		t.Skipf("live host-memory probe unavailable: %v", err)
	}
	if total < 4096 {
		t.Fatalf("HostTotalMiB = %d, %v (want a realistic RAM size)", total, err)
	}
	free, err := HostFreeMiB()
	if err != nil || free < 0 || free > total {
		t.Fatalf("HostFreeMiB = %d, %v (want 0..total)", free, err)
	}
	t.Logf("host: total=%dMiB free=%dMiB", total, free)
}
