//go:build linux

package helper

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/bgp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type stubLinuxRouteOps struct {
	replaced   []*netlink.Route
	deleted    []*netlink.Route
	replaceErr error
	deleteErr  error
}

func (s *stubLinuxRouteOps) Replace(route *netlink.Route) error {
	s.replaced = append(s.replaced, route)
	return s.replaceErr
}

func (s *stubLinuxRouteOps) Delete(route *netlink.Route) error {
	s.deleted = append(s.deleted, route)
	return s.deleteErr
}

type fakeLinuxSpeaker struct {
	stops int
}

func (f *fakeLinuxSpeaker) Stop() {
	f.stops++
}

func TestRouteFIBAddHostRouteReplacesIPv4HostRoute(t *testing.T) {
	original := linuxRoutes
	routes := &stubLinuxRouteOps{}
	linuxRoutes = routes
	t.Cleanup(func() { linuxRoutes = original })

	if err := (routeFIB{}).AddHostRoute("172.30.0.200/32", "172.30.0.2"); err != nil {
		t.Fatal(err)
	}
	if len(routes.replaced) != 1 {
		t.Fatalf("Replace() calls = %d, want 1", len(routes.replaced))
	}
	route := routes.replaced[0]
	if got := route.Dst.String(); got != "172.30.0.200/32" {
		t.Fatalf("route destination = %q, want 172.30.0.200/32", got)
	}
	if got := route.Gw.String(); got != "172.30.0.2" {
		t.Fatalf("route gateway = %q, want 172.30.0.2", got)
	}
}

func TestRouteFIBAddHostRouteRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		prefix  string
		nexthop string
		want    string
	}{
		{name: "bad prefix", prefix: "not-a-cidr", nexthop: "172.30.0.2", want: "parse route prefix"},
		{name: "not host route", prefix: "172.30.0.0/24", nexthop: "172.30.0.2", want: "not an IPv4 host route"},
		{name: "bad next hop", prefix: "172.30.0.200/32", nexthop: "not-an-ip", want: "parse route next hop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := (routeFIB{}).AddHostRoute(tc.prefix, tc.nexthop)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AddHostRoute(%q, %q) error = %v, want substring %q", tc.prefix, tc.nexthop, err, tc.want)
			}
			var fibErr *RouteFIBError
			if !errors.As(err, &fibErr) {
				t.Fatalf("AddHostRoute(%q, %q) error = %T, want RouteFIBError", tc.prefix, tc.nexthop, err)
			}
		})
	}
}

func TestRouteFIBErrorsExposeOperationAndCause(t *testing.T) {
	original := linuxRoutes
	routes := &stubLinuxRouteOps{replaceErr: unix.EPERM}
	linuxRoutes = routes
	t.Cleanup(func() { linuxRoutes = original })

	err := (routeFIB{}).AddHostRoute("172.30.0.200/32", "172.30.0.2")
	var fibErr *RouteFIBError
	if !errors.As(err, &fibErr) {
		t.Fatalf("AddHostRoute() error = %T, want RouteFIBError", err)
	}
	if fibErr.Operation != "replace" || fibErr.Prefix != "172.30.0.200/32" || fibErr.Nexthop != "172.30.0.2" {
		t.Fatalf("RouteFIBError = %+v, want replace metadata", fibErr)
	}
	if !errors.Is(err, unix.EPERM) {
		t.Fatalf("AddHostRoute() error = %v, want unix.EPERM", err)
	}
}

func TestRouteFIBDeleteHostRouteIgnoresMissingRoute(t *testing.T) {
	original := linuxRoutes
	routes := &stubLinuxRouteOps{deleteErr: unix.ENOENT}
	linuxRoutes = routes
	t.Cleanup(func() { linuxRoutes = original })

	if err := (routeFIB{}).DeleteHostRoute("172.30.0.200/32"); err != nil {
		t.Fatal(err)
	}
	if len(routes.deleted) != 1 {
		t.Fatalf("Delete() calls = %d, want 1", len(routes.deleted))
	}
	route := routes.deleted[0]
	if got := route.Dst.String(); got != "172.30.0.200/32" {
		t.Fatalf("route destination = %q, want 172.30.0.200/32", got)
	}
	if route.Gw != nil {
		t.Fatalf("DeleteHostRoute() gateway = %v, want nil", route.Gw)
	}
}

func TestEnableBGPLinuxStartsSpeakerIdempotently(t *testing.T) {
	original := startLinuxBGPSpeaker
	var calls int
	fake := &fakeLinuxSpeaker{}
	startLinuxBGPSpeaker = func(localASN, peerASN uint32, gatewayIP, peerCIDR string, fib bgp.FIB) (bgpSpeaker, error) {
		calls++
		if localASN != 64512 || peerASN != 64600 {
			t.Fatalf("start args ASN = (%d, %d), want (64512, 64600)", localASN, peerASN)
		}
		if gatewayIP != "172.30.7.1" {
			t.Fatalf("gatewayIP = %q, want 172.30.7.1", gatewayIP)
		}
		if peerCIDR != "172.30.7.0/24" {
			t.Fatalf("peerCIDR = %q, want 172.30.7.0/24", peerCIDR)
		}
		if _, ok := fib.(routeFIB); !ok {
			t.Fatalf("fib type = %T, want routeFIB", fib)
		}
		return fake, nil
	}
	t.Cleanup(func() { startLinuxBGPSpeaker = original })

	server := &Server{}
	args, err := json.Marshal(bgpArgs{Cluster: "demo", SubnetIndex: 7, LocalASN: 64512, PeerASN: 64600})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.enableBGP(args); err != nil {
		t.Fatal(err)
	}
	if err := server.enableBGP(args); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("startLinuxBGPSpeaker() calls = %d, want 1", calls)
	}
	if server.speakers["demo"] != fake {
		t.Fatalf("server speaker = %v, want fake speaker", server.speakers["demo"])
	}
}

func TestEnableBGPLinuxWrapsStartError(t *testing.T) {
	original := startLinuxBGPSpeaker
	startErr := errors.New("bind :179: permission denied")
	startLinuxBGPSpeaker = func(uint32, uint32, string, string, bgp.FIB) (bgpSpeaker, error) {
		return nil, startErr
	}
	t.Cleanup(func() { startLinuxBGPSpeaker = original })

	server := &Server{}
	args, err := json.Marshal(bgpArgs{Cluster: "demo", SubnetIndex: 0, LocalASN: 64512, PeerASN: 64600})
	if err != nil {
		t.Fatal(err)
	}
	err = server.enableBGP(args)
	if !errors.Is(err, startErr) || !strings.Contains(err.Error(), "start bgp speaker for demo") {
		t.Fatalf("enableBGP() error = %v, want wrapped start error", err)
	}
}

func TestDisableBGPLinuxStopsSpeakerIdempotently(t *testing.T) {
	t.Parallel()

	fake := &fakeLinuxSpeaker{}
	server := &Server{speakers: map[string]bgpSpeaker{"demo": fake}}
	args, err := json.Marshal(bgpArgs{Cluster: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.disableBGP(args); err != nil {
		t.Fatal(err)
	}
	if err := server.disableBGP(args); err != nil {
		t.Fatal(err)
	}
	if fake.stops != 1 {
		t.Fatalf("speaker Stop() calls = %d, want 1", fake.stops)
	}
	if _, ok := server.speakers["demo"]; ok {
		t.Fatal("speaker still registered after disable")
	}
}
