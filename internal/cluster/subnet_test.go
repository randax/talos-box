package cluster

import (
	"net"
	"strings"
	"testing"
)

func TestLowestUsableSubnetIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		clusters   []Cluster
		interfaces []HostInterface
		route      func(net.IP) (HostRoute, error)
		wantIndex  int
		wantWarn   string
		wantErr    string
	}{
		{
			name:      "clean host",
			route:     staticRoute("en0", "0.0.0.0/0"),
			wantIndex: 0,
		},
		{
			name: "foreign interface skips index",
			interfaces: []HostInterface{
				{Name: "en7", Addrs: []net.Addr{hostAddress("172.30.0.50/24")}},
			},
			route:     staticRoute("en0", "0.0.0.0/0"),
			wantIndex: 1,
		},
		{
			name: "all indexes overlap",
			interfaces: []HostInterface{
				{Name: "en7", Addrs: []net.Addr{hostAddress("172.30.0.50/16")}},
			},
			route:   staticRoute("en0", "0.0.0.0/0"),
			wantErr: "all cluster subnets overlap existing host interfaces or routes",
		},
		{
			name:      "broad VPN route warns",
			route:     staticRoute("utun4", "172.16.0.0/12"),
			wantIndex: 0,
			wantWarn:  "utun4",
		},
		{
			name:      "full tunnel VPN default route warns",
			route:     staticRoute("utun8", "0.0.0.0/0"),
			wantIndex: 0,
			wantWarn:  "utun8",
		},
		{
			name:      "no route is clean",
			route:     func(net.IP) (HostRoute, error) { return HostRoute{}, nil },
			wantIndex: 0,
		},
		{
			name: "unallocated vmnet bridge is a collision",
			interfaces: []HostInterface{
				{Name: "bridge100", Addrs: []net.Addr{hostAddress("172.30.0.1/24")}},
			},
			route:     staticRoute("en0", "0.0.0.0/0"),
			wantIndex: 1,
		},
		{
			name: "foreign bridge address is not ignored",
			interfaces: []HostInterface{
				{Name: "bridge0", Addrs: []net.Addr{hostAddress("172.30.0.1/24")}},
			},
			route:     staticRoute("en0", "0.0.0.0/0"),
			wantIndex: 1,
		},
		{
			name:      "specific foreign route skips index",
			route:     routeByThirdOctet(map[byte]HostRoute{0: routeValue("utun4", "172.30.0.0/24")}),
			wantIndex: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			sources := SubnetSources{
				Interfaces: func() ([]HostInterface, error) { return test.interfaces, nil },
				Route:      test.route,
			}
			gotIndex, gotWarning, err := LowestUsableSubnetIndex(test.clusters, sources)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("LowestUsableSubnetIndex() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotIndex != test.wantIndex {
				t.Errorf("index = %d, want %d", gotIndex, test.wantIndex)
			}
			if !strings.Contains(gotWarning, test.wantWarn) {
				t.Errorf("warning = %q, want containing %q", gotWarning, test.wantWarn)
			}
			if test.wantWarn != "" && !strings.Contains(gotWarning, "capture cluster traffic") {
				t.Errorf("warning = %q, want traffic-capture risk", gotWarning)
			}
		})
	}
}

func TestCheckSubnetIndexAllowsExistingTalosBoxBridge(t *testing.T) {
	t.Parallel()

	sources := SubnetSources{
		Interfaces: func() ([]HostInterface, error) {
			return []HostInterface{{Name: "bridge100", Addrs: []net.Addr{hostAddress("172.30.0.1/24")}}}, nil
		},
		Route: staticRoute("bridge100", "172.30.0.0/24"),
	}
	warning, err := CheckSubnetIndex(0, sources)
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Fatalf("warning = %q, want empty", warning)
	}
}

func TestRouteNotFound(t *testing.T) {
	t.Parallel()

	for _, output := range []string{
		"route: writing to routing socket: not in table\n",
		"route: route has not been found\n",
	} {
		if !routeNotFound([]byte(output)) {
			t.Errorf("routeNotFound(%q) = false", output)
		}
	}
	if routeNotFound([]byte("route: socket: operation not permitted\n")) {
		t.Error("permission failure must not be treated as a missing route")
	}
}

func TestParseHostRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		output      string
		wantIface   string
		wantNetwork string
	}{
		{
			name:        "default route",
			output:      "route to: 172.30.0.2\ndestination: default\ngateway: 10.0.0.1\ninterface: en0\n",
			wantIface:   "en0",
			wantNetwork: "0.0.0.0/0",
		},
		{
			name:        "broad VPN route",
			output:      "route to: 172.30.0.2\ndestination: 172.16.0.0\nmask: 255.240.0.0\ninterface: utun7\n",
			wantIface:   "utun7",
			wantNetwork: "172.16.0.0/12",
		},
		{
			name:        "hex mask",
			output:      "destination: 172.30.4.0\nmask: 0xffffff00\ninterface: en7\n",
			wantIface:   "en7",
			wantNetwork: "172.30.4.0/24",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseHostRoute([]byte(test.output), net.ParseIP("172.30.0.2"))
			if err != nil {
				t.Fatal(err)
			}
			if got.Interface != test.wantIface || got.Network.String() != test.wantNetwork {
				t.Fatalf("parseHostRoute() = %+v, want interface %s network %s", got, test.wantIface, test.wantNetwork)
			}
		})
	}
}

func hostAddress(cidr string) net.Addr {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	network.IP = ip
	return network
}

func routeValue(name, cidr string) HostRoute {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return HostRoute{Interface: name, Network: network}
}

func staticRoute(name, cidr string) func(net.IP) (HostRoute, error) {
	value := routeValue(name, cidr)
	return func(net.IP) (HostRoute, error) { return value, nil }
}

func routeByThirdOctet(routes map[byte]HostRoute) func(net.IP) (HostRoute, error) {
	defaultRoute := routeValue("en0", "0.0.0.0/0")
	return func(ip net.IP) (HostRoute, error) {
		if route, ok := routes[ip.To4()[2]]; ok {
			return route, nil
		}
		return defaultRoute, nil
	}
}
