//go:build darwin

package balloon

import (
	"context"
	"errors"
	"testing"
)

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

func TestHostFreeMiBContextHonoursDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if free, err := HostFreeMiBContext(ctx); err == nil {
		t.Fatalf("HostFreeMiBContext with a cancelled context = %d, nil (want an error)", free)
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("HostFreeMiBContext error = %v (want context.Canceled)", err)
	}
}
