//go:build darwin

package balloon

import (
	"context"
	"errors"
	"testing"
)

func TestHostFreeMiBContextHonoursDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if free, err := HostFreeMiBContext(ctx); err == nil {
		t.Fatalf("HostFreeMiBContext with a cancelled context = %d, nil (want an error)", free)
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("HostFreeMiBContext error = %v (want context.Canceled)", err)
	}
}
