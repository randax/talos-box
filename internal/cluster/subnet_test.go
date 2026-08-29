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
			name:      "ordinary default route is clean",
			route:     staticRoute("default-link", "0.0.0.0/0"),
			wantIndex: 0,
		},
		{
			name:      "tunnel default route warns",
			route:     staticRoute("private-link", "0.0.0.0/0", true),
			wantIndex: 0,
			wantWarn:  "private-link",
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
			name:      "broad non-default route warns without tunnel signal",
			route:     staticRoute("broad-link", "172.16.0.0/12"),
			wantIndex: 0,
			wantWarn:  "broad-link",
		},
		{
			name:      "broad non-default route warns with tunnel signal",
			route:     staticRoute("broad-private-link", "172.16.0.0/12", true),
			wantIndex: 0,
			wantWarn:  "broad-private-link",
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
			name:      "specific route conflicts without tunnel signal",
			route:     routeByThirdOctet(map[byte]HostRoute{0: routeValue("specific-link", "172.30.0.0/24")}),
			wantIndex: 1,
		},
		{
			name:      "specific route conflicts with tunnel signal",
			route:     routeByThirdOctet(map[byte]HostRoute{0: routeValue("specific-private-link", "172.30.0.0/24", true)}),
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

func TestCheckSubnetIndexAllowsExistingLinuxTalosBoxBridge(t *testing.T) {
	t.Parallel()

	sources := SubnetSources{
		Interfaces: func() ([]HostInterface, error) {
			return []HostInterface{{Name: "br-tbx9", Addrs: []net.Addr{hostAddress("172.30.9.1/24")}}}, nil
		},
		Route: staticRoute("br-tbx9", "172.30.9.0/24"),
	}
	warning, err := CheckSubnetIndex(9, sources)
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Fatalf("warning = %q, want empty", warning)
	}
}

func TestCheckSubnetIndexRejectsForeignBridgeRoute(t *testing.T) {
	t.Parallel()

	sources := SubnetSources{
		Interfaces: func() ([]HostInterface, error) { return nil, nil },
		Route:      staticRoute("bridge0", "172.30.9.0/24"),
	}
	_, err := CheckSubnetIndex(9, sources)
	if err == nil || !strings.Contains(err.Error(), "bridge0") {
		t.Fatalf("CheckSubnetIndex() error = %v, want foreign bridge route collision", err)
	}
}

func TestCheckSubnetIndexRejectsDifferentTalosBoxBridgeRoute(t *testing.T) {
	t.Parallel()

	sources := SubnetSources{
		Interfaces: func() ([]HostInterface, error) { return nil, nil },
		Route:      staticRoute("bridge108", "172.30.7.0/24"),
	}
	_, err := CheckSubnetIndex(7, sources)
	if err == nil || !strings.Contains(err.Error(), "bridge108") {
		t.Fatalf("CheckSubnetIndex() error = %v, want different-bridge route collision", err)
	}
}

// A vmnet bridge is named by the host, so a cluster's own bridge for subnet
// index 0 may well be bridge101. An attached subnet must not be re-validated
// against it, while cluster create still refuses that same collision.
func TestAttachedSubnetWarningIgnoresOwnBridgeUnderAnyName(t *testing.T) {
	t.Parallel()

	sources := SubnetSources{
		Interfaces: func() ([]HostInterface, error) {
			return []HostInterface{{Name: "bridge101", Addrs: []net.Addr{hostAddress("172.30.0.1/24")}}}, nil
		},
		Route: staticRoute("bridge101", "172.30.0.0/24"),
	}
	warning, err := AttachedSubnetWarning(0, sources)
	if err != nil {
		t.Fatalf("AttachedSubnetWarning() error = %v, want the cluster's own attachment to be accepted", err)
	}
	if warning != "" {
		t.Fatalf("warning = %q, want empty", warning)
	}
	if _, err := CheckSubnetIndex(0, sources); err == nil {
		t.Fatal("CheckSubnetIndex() error = nil, want create to still reject a genuine collision")
	}
}

// The gateway address alone does not prove ownership: after a clean stop the
// cluster's own bridge is gone, and a foreign interface may hold exactly that
// address. Exempting it silently would hide the collision that breaks the
// cluster, so it is reported — as a warning, never as a refusal.
func TestAttachedSubnetWarningReportsAForeignGatewaySquatter(t *testing.T) {
	t.Parallel()

	sources := SubnetSources{
		Interfaces: func() ([]HostInterface, error) {
			return []HostInterface{{Name: "docker0", Addrs: []net.Addr{hostAddress("172.30.0.1/24")}}}, nil
		},
		Route: staticRoute("docker0", "172.30.0.0/24"),
	}
	warning, err := AttachedSubnetWarning(0, sources)
	if err != nil {
		t.Fatalf("AttachedSubnetWarning() error = %v, want a warning instead", err)
	}
	if !strings.Contains(warning, "docker0") || !strings.Contains(warning, "conflicts") {
		t.Fatalf("warning = %q, want a conflict warning naming docker0", warning)
	}
}

// bridge0 is the user's own Thunderbolt/Ethernet bridge, not a vmnet one.
func TestAttachedSubnetWarningReportsAUserBridgeHoldingTheGateway(t *testing.T) {
	t.Parallel()

	sources := SubnetSources{
		Interfaces: func() ([]HostInterface, error) {
			return []HostInterface{{Name: "bridge0", Addrs: []net.Addr{hostAddress("172.30.0.1/24")}}}, nil
		},
		Route: staticRoute("bridge0", "172.30.0.0/24"),
	}
	warning, err := AttachedSubnetWarning(0, sources)
	if err != nil {
		t.Fatalf("AttachedSubnetWarning() error = %v, want a warning instead", err)
	}
	if !strings.Contains(warning, "bridge0") || !strings.Contains(warning, "conflicts") {
		t.Fatalf("warning = %q, want a conflict warning naming bridge0", warning)
	}
}

func TestAttachedSubnetWarningIgnoresTheLinuxOwnBridgeName(t *testing.T) {
	t.Parallel()

	sources := SubnetSources{
		Interfaces: func() ([]HostInterface, error) {
			return []HostInterface{{Name: LinuxBridgeName(3), Addrs: []net.Addr{hostAddress("172.30.3.1/24")}}}, nil
		},
		Route: staticRoute(LinuxBridgeName(3), "172.30.3.0/24"),
	}
	warning, err := AttachedSubnetWarning(3, sources)
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Fatalf("warning = %q, want empty", warning)
	}
}

func TestAttachedSubnetWarningStillReportsBroadRoute(t *testing.T) {
	t.Parallel()

	sources := SubnetSources{
		Interfaces: func() ([]HostInterface, error) { return nil, nil },
		Route:      staticRoute("utun4", "172.16.0.0/12"),
	}
	warning, err := AttachedSubnetWarning(0, sources)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning, "utun4") {
		t.Fatalf("warning = %q, want containing %q", warning, "utun4")
	}
}

func TestSelectMostSpecificRouteSkipsOwnedBridgeRoute(t *testing.T) {
	t.Parallel()

	destination := net.ParseIP("172.30.9.2")
	routes := []HostRoute{
		mustHostRoute(t, "br-tbx9", "172.30.9.0/24"),
		mustHostRoute(t, "vpn0", "172.16.0.0/12"),
		mustHostRoute(t, "eth0", "0.0.0.0/0"),
	}
	got := selectMostSpecificRoute(destination, routes, map[string]bool{"br-tbx9": true})
	if got.Interface != "vpn0" || got.Network.String() != "172.16.0.0/12" {
		t.Fatalf("selected route = %+v, want vpn0 172.16.0.0/12", got)
	}
}

func TestSelectMostSpecificRouteFallsBackToDefaultAfterOwnedBridge(t *testing.T) {
	t.Parallel()

	destination := net.ParseIP("172.30.9.2")
	routes := []HostRoute{
		mustHostRoute(t, "br-tbx9", "172.30.9.0/24"),
		mustHostRoute(t, "eth0", "0.0.0.0/0"),
	}
	got := selectMostSpecificRoute(destination, routes, map[string]bool{"br-tbx9": true})
	if got.Interface != "eth0" || got.Network.String() != "0.0.0.0/0" {
		t.Fatalf("selected route = %+v, want eth0 0.0.0.0/0", got)
	}
}

func mustHostRoute(t *testing.T, name, cidr string) HostRoute {
	t.Helper()
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return HostRoute{Interface: name, Network: network}
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

func routeValue(name, cidr string, looksLikeTunnel ...bool) HostRoute {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	tunnel := len(looksLikeTunnel) > 0 && looksLikeTunnel[0]
	return HostRoute{Interface: name, Network: network, LooksLikeTunnel: tunnel}
}

func staticRoute(name, cidr string, looksLikeTunnel ...bool) func(net.IP) (HostRoute, error) {
	value := routeValue(name, cidr, looksLikeTunnel...)
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

// A foreign interface squatting on an attached subnet cannot be resolved by a
// node mutation, but the operator still needs to hear about it.
func TestAttachedSubnetWarningSurfacesForeignConflict(t *testing.T) {
	t.Parallel()

	sources := SubnetSources{
		Interfaces: func() ([]HostInterface, error) {
			return []HostInterface{
				{Name: "bridge101", Addrs: []net.Addr{hostAddress("172.30.0.1/24")}},
				{Name: "utun7", Addrs: []net.Addr{hostAddress("172.30.0.5/24")}},
			}, nil
		},
		Route: staticRoute("bridge101", "172.30.0.0/24"),
	}
	warning, err := AttachedSubnetWarning(0, sources)
	if err != nil {
		t.Fatalf("AttachedSubnetWarning() error = %v, want a warning instead", err)
	}
	if !strings.Contains(warning, "utun7") || !strings.Contains(warning, "conflicts") {
		t.Fatalf("warning = %q, want a foreign-conflict warning naming utun7", warning)
	}
}

// A destroyed cluster's bridge keeps the subnet's gateway address, and
// allocation reads that address as a collision — which is why a leaked bridge
// made every create climb to a new index (#445). Removing the bridge is what
// frees the index; allocation itself needs no memory of it.
func TestLowestUsableSubnetIndexReusesAFreedIndexOnceItsBridgeIsGone(t *testing.T) {
	t.Parallel()

	leaked := SubnetSources{
		Interfaces: func() ([]HostInterface, error) {
			return []HostInterface{{Name: "br-tbx0", Addrs: []net.Addr{hostAddress("172.30.0.1/24")}}}, nil
		},
		Route: routeByThirdOctet(map[byte]HostRoute{0: routeValue("br-tbx0", "172.30.0.0/24")}),
	}
	index, _, err := LowestUsableSubnetIndex(nil, leaked)
	if err != nil {
		t.Fatal(err)
	}
	if index != 1 {
		t.Fatalf("LowestUsableSubnetIndex() with a leaked bridge = %d, want 1", index)
	}

	torndown := SubnetSources{
		Interfaces: func() ([]HostInterface, error) { return nil, nil },
		Route:      routeByThirdOctet(nil),
	}
	index, _, err = LowestUsableSubnetIndex(nil, torndown)
	if err != nil {
		t.Fatal(err)
	}
	if index != 0 {
		t.Fatalf("LowestUsableSubnetIndex() after teardown = %d, want 0", index)
	}
}
