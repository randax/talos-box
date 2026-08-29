//go:build !darwin && !linux

package cluster

import (
	"net"
	"testing"
)

func TestStubSystemRouteHasNoTunnelSignal(t *testing.T) {
	t.Parallel()

	got, err := systemRoute(net.ParseIP("172.30.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Interface != "" || got.Network != nil || got.LooksLikeTunnel {
		t.Fatalf("systemRoute() = %+v, want zero route", got)
	}
}
