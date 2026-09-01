package wsl

import (
	"errors"
	"net"
	"testing"
)

func TestNATPrefixUsesTheLowestMetricDefaultRouteInterface(t *testing.T) {
	t.Parallel()

	routes := []byte("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth2\t010013AC\t010013AC\t0003\t0\t0\t1\t00000000\t0\t0\t0\n" +
		"eth3\t00000000\t010013AC\t0003\t0\t0\t2\tF0FFFFFF\t0\t0\t0\n" +
		"eth1\t00000000\t010013AC\t0003\t0\t0\t200\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t010013AC\t0003\t0\t0\t100\t00000000\t0\t0\t0\n")
	got, err := natPrefixFrom(
		func(path string) ([]byte, error) {
			if path != procNetRoute {
				t.Fatalf("read %q, want %q", path, procNetRoute)
			}
			return routes, nil
		},
		func(name string) ([]net.Addr, error) {
			if name != "eth0" {
				t.Fatalf("interface = %q, want eth0", name)
			}
			return []net.Addr{&net.IPNet{IP: net.ParseIP("172.19.146.224"), Mask: net.CIDRMask(20, 32)}}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "172.19.144.0/20" {
		t.Fatalf("prefix = %q, want 172.19.144.0/20", got)
	}
}

func TestLinuxRouteIPv4DecodesLittleEndianValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  string
	}{
		{value: "010013AC", want: "172.19.0.1"},
		{value: "F0FFFFFF", want: "255.255.255.240"},
	}
	for _, tt := range tests {
		if got, err := linuxRouteIPv4(tt.value); err != nil || got.String() != tt.want {
			t.Errorf("linuxRouteIPv4(%q) = %v, %v; want %s", tt.value, got, err, tt.want)
		}
	}
	if _, err := linuxRouteIPv4("00E0FF"); err == nil {
		t.Fatal("linuxRouteIPv4(short value) = nil error, want length error")
	}
}

func TestNATPrefixRejectsUnusableRouteOrAddress(t *testing.T) {
	t.Parallel()

	header := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\n"
	defaultRoute := "eth0\t00000000\t010013AC\t0003\t0\t0\t100\t00000000\n"
	tests := []struct {
		name   string
		routes string
		addrs  func(string) ([]net.Addr, error)
	}{
		{name: "malformed route row", routes: header + "eth0 bad\n", addrs: unusedInterfaceAddrs(t)},
		{name: "no default", routes: header + "eth0\t000013AC\t010013AC\t0003\t0\t0\t100\t00FFFFFF\n", addrs: unusedInterfaceAddrs(t)},
		{name: "down default", routes: header + "eth0\t00000000\t010013AC\t0002\t0\t0\t100\t00000000\n", addrs: unusedInterfaceAddrs(t)},
		{name: "missing interface", routes: header + defaultRoute, addrs: func(string) ([]net.Addr, error) { return nil, errors.New("missing") }},
		{name: "IPv6 only", routes: header + defaultRoute, addrs: fixedInterfaceAddrs("2001:db8::1/64")},
		{name: "loopback", routes: header + defaultRoute, addrs: fixedInterfaceAddrs("127.0.0.1/8")},
		{name: "host only", routes: header + defaultRoute, addrs: fixedInterfaceAddrs("172.19.146.224/32")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if prefix, err := natPrefixFrom(func(string) ([]byte, error) { return []byte(tt.routes), nil }, tt.addrs); err == nil {
				t.Fatalf("natPrefixFrom() = %q, nil; want error", prefix)
			}
		})
	}
}

func fixedInterfaceAddrs(cidr string) func(string) ([]net.Addr, error) {
	return func(string) ([]net.Addr, error) {
		ip, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, err
		}
		network.IP = ip
		return []net.Addr{network}, nil
	}
}

func unusedInterfaceAddrs(t *testing.T) func(string) ([]net.Addr, error) {
	t.Helper()
	return func(string) ([]net.Addr, error) {
		t.Fatal("interface addresses read without a usable default route")
		return nil, nil
	}
}
