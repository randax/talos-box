//go:build darwin && live

// Run live host-memory probes with:
// go test -tags live ./internal/hostmem ./internal/balloon
package hostmem

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestLiveSystemSnapshot(t *testing.T) {
	if _, err := exec.LookPath("vm_stat"); err != nil {
		t.Skip("vm_stat is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := systemSnapshot(ctx)
	if err != nil {
		t.Skipf("live host-memory probe unavailable: %v", err)
	}
	if snapshot.TotalMiB <= 0 || snapshot.AvailableMiB < 0 {
		t.Fatalf("implausible live snapshot: %+v", snapshot)
	}
}
