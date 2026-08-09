package helper

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

func TestStartLinuxAttachmentRejectsCollidingSubnetWithHint(t *testing.T) {
	t.Parallel()

	ops := &fakeLinuxNetworkOps{
		links: []linuxLinkState{
			{Name: bridgeNameForSubnet(4), Addrs: []net.Addr{mustCIDR("172.30.4.1/24")}},
		},
		routes: map[string]cluster.HostRoute{
			"172.30.7.2": mustRoute("utun7", "172.30.7.0/24"),
			"172.30.0.2": mustRoute("en0", "0.0.0.0/0"),
			"172.30.1.2": mustRoute("en0", "0.0.0.0/0"),
			"172.30.2.2": mustRoute("en0", "0.0.0.0/0"),
			"172.30.3.2": mustRoute("en0", "0.0.0.0/0"),
			"172.30.4.2": mustRoute("br-tbx4", "172.30.4.0/24"),
			"172.30.5.2": mustRoute("en0", "0.0.0.0/0"),
			"172.30.6.2": mustRoute("en0", "0.0.0.0/0"),
			"172.30.8.2": mustRoute("en0", "0.0.0.0/0"),
		},
	}
	nft := &fakeLinuxNFTConverger{}

	_, err := startLinuxAttachment(ops, nft, 7, "demo", "cp-1")
	if err == nil {
		t.Fatal("startLinuxAttachment() error = nil, want conflict")
	}
	for _, fragment := range []string{"172.30.7.0/24", "utun7", "try subnet index 0"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error = %q, want %q", err, fragment)
		}
	}
	if len(ops.calls) != 0 {
		t.Fatalf("network operations were attempted after preflight failure: %v", ops.calls)
	}
	if nft.calls != 0 {
		t.Fatalf("nft converge calls = %d, want 0", nft.calls)
	}
}

func TestStartLinuxAttachmentRejectsBroaderVPNRouteWithHint(t *testing.T) {
	t.Parallel()

	ops := &fakeLinuxNetworkOps{
		routes: map[string]cluster.HostRoute{
			"172.30.7.2": mustRoute("utun7", "172.30.6.0/23"),
			"172.30.0.2": mustRoute("en0", "0.0.0.0/0"),
		},
	}

	_, err := startLinuxAttachment(ops, &fakeLinuxNFTConverger{}, 7, "demo", "cp-1")
	if err == nil {
		t.Fatal("startLinuxAttachment() error = nil, want broad VPN collision")
	}
	for _, fragment := range []string{"172.30.7.0/24", "utun7", "try subnet index 0"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error = %q, want %q", err, fragment)
		}
	}
}

func TestStartLinuxAttachmentConvergesBridgeAndReturnsTap(t *testing.T) {
	t.Parallel()

	tap, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tap.Close() })

	ops := &fakeLinuxNetworkOps{
		links: []linuxLinkState{
			{Name: bridgeNameForSubnet(2), Addrs: []net.Addr{mustCIDR("172.30.2.1/24")}},
		},
		defaultRoute: mustRoute("en0", "0.0.0.0/0"),
		tap:          tap,
	}
	nft := &fakeLinuxNFTConverger{}

	got, err := startLinuxAttachment(ops, nft, 3, "demo", "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != tap {
		t.Fatalf("tap file = %v, want %v", got, tap)
	}
	wantCalls := []string{
		"ipv4-forwarding",
		"bridge:" + bridgeNameForSubnet(3),
		"stp:" + bridgeNameForSubnet(3) + "=false",
		"addr:" + bridgeNameForSubnet(3) + "=172.30.3.1/24",
		"up:" + bridgeNameForSubnet(3),
		"iface-forwarding:" + bridgeNameForSubnet(3),
		"tap:" + tapNameForNode(3, "demo", "cp-1") + "->" + bridgeNameForSubnet(3),
	}
	if !reflect.DeepEqual(ops.calls, wantCalls) {
		t.Fatalf("network calls = %#v, want %#v", ops.calls, wantCalls)
	}
	if got := nft.subnetIndexes; !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("nft subnet indexes = %v, want [2 3]", got)
	}
}

func TestConvergeLinuxManagedStateReassertsExistingBridges(t *testing.T) {
	t.Parallel()

	ops := &fakeLinuxNetworkOps{
		links: []linuxLinkState{
			{Name: bridgeNameForSubnet(7), Addrs: []net.Addr{mustCIDR("172.30.7.1/24")}},
			{Name: "eth0", Addrs: []net.Addr{mustCIDR("10.0.0.5/24")}},
			{Name: bridgeNameForSubnet(3), Addrs: []net.Addr{mustCIDR("172.30.3.1/24")}},
		},
	}
	nft := &fakeLinuxNFTConverger{}

	if err := convergeLinuxManagedState(ops, nft); err != nil {
		t.Fatal(err)
	}

	wantCalls := []string{
		"ipv4-forwarding",
		"bridge:" + bridgeNameForSubnet(3),
		"stp:" + bridgeNameForSubnet(3) + "=false",
		"addr:" + bridgeNameForSubnet(3) + "=172.30.3.1/24",
		"up:" + bridgeNameForSubnet(3),
		"iface-forwarding:" + bridgeNameForSubnet(3),
		"bridge:" + bridgeNameForSubnet(7),
		"stp:" + bridgeNameForSubnet(7) + "=false",
		"addr:" + bridgeNameForSubnet(7) + "=172.30.7.1/24",
		"up:" + bridgeNameForSubnet(7),
		"iface-forwarding:" + bridgeNameForSubnet(7),
		"managed-taps",
	}
	if !reflect.DeepEqual(ops.calls, wantCalls) {
		t.Fatalf("network calls = %#v, want %#v", ops.calls, wantCalls)
	}
	if got := nft.subnetIndexes; !reflect.DeepEqual(got, []int{3, 7}) {
		t.Fatalf("nft subnet indexes = %v, want [3 7]", got)
	}
}

func TestConvergeLinuxManagedStateRecreatesBridgeForSurvivingTap(t *testing.T) {
	t.Parallel()

	ops := &fakeLinuxNetworkOps{
		links: []linuxLinkState{{Name: tapNameForNode(12, "demo", "cp-1")}},
	}
	nft := &fakeLinuxNFTConverger{}
	if err := convergeLinuxManagedState(ops, nft); err != nil {
		t.Fatal(err)
	}
	if got, want := ops.calls, []string{
		"ipv4-forwarding",
		"bridge:" + bridgeNameForSubnet(12),
		"stp:" + bridgeNameForSubnet(12) + "=false",
		"addr:" + bridgeNameForSubnet(12) + "=172.30.12.1/24",
		"up:" + bridgeNameForSubnet(12),
		"iface-forwarding:" + bridgeNameForSubnet(12),
		"managed-taps",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("network calls = %#v, want %#v", got, want)
	}
	if got := nft.subnetIndexes; !reflect.DeepEqual(got, []int{12}) {
		t.Fatalf("nft subnet indexes = %v, want [12]", got)
	}
}

func TestLinuxTapNameEncodesSubnetAndFitsInterfaceLimit(t *testing.T) {
	t.Parallel()

	name := tapNameForNode(255, "a-cluster-with-a-long-name", "a-node-with-a-long-name")
	if len(name) >= 16 {
		t.Fatalf("tap name %q is %d bytes, want less than IFNAMSIZ", name, len(name))
	}
	index, ok := subnetIndexFromTapName(name)
	if !ok || index != 255 {
		t.Fatalf("subnetIndexFromTapName(%q) = %d, %t, want 255, true", name, index, ok)
	}
	for _, foreign := range []string{"tbx255", "tbx256-deadbeef", "tbx7-not-hex!", "tap-tbx7-deadbeef"} {
		if _, ok := subnetIndexFromTapName(foreign); ok {
			t.Fatalf("subnetIndexFromTapName(%q) accepted foreign interface", foreign)
		}
	}
}

func TestLinuxClusterSourcesTreatManagedBridgeAsTalosBoxBridge(t *testing.T) {
	t.Parallel()

	sources := linuxClusterSources(linuxSubnetInspector{
		Interfaces: func() ([]linuxLinkState, error) {
			return []linuxLinkState{{Name: bridgeNameForSubnet(9), Addrs: []net.Addr{mustCIDR("172.30.9.1/24")}}}, nil
		},
		Route: func(net.IP) (cluster.HostRoute, error) {
			return mustRoute("bridge109", "172.30.9.0/24"), nil
		},
	})
	warning, err := cluster.CheckSubnetIndex(9, sources)
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Fatalf("warning = %q, want empty", warning)
	}
}

type fakeLinuxNetworkOps struct {
	links        []linuxLinkState
	routes       map[string]cluster.HostRoute
	defaultRoute cluster.HostRoute
	tap          *os.File
	calls        []string
}

func (f *fakeLinuxNetworkOps) ListLinks() ([]linuxLinkState, error) { return f.links, nil }
func (f *fakeLinuxNetworkOps) Route(ip net.IP) (cluster.HostRoute, error) {
	if route, ok := f.routes[ip.String()]; ok {
		return route, nil
	}
	if f.defaultRoute.Interface != "" || f.defaultRoute.Network != nil {
		return f.defaultRoute, nil
	}
	return cluster.HostRoute{}, errors.New("missing route")
}
func (f *fakeLinuxNetworkOps) EnsureIPv4Forwarding() error {
	f.calls = append(f.calls, "ipv4-forwarding")
	return nil
}
func (f *fakeLinuxNetworkOps) EnsureInterfaceForwarding(name string) error {
	f.calls = append(f.calls, "iface-forwarding:"+name)
	return nil
}
func (f *fakeLinuxNetworkOps) EnsureManagedTaps() error {
	f.calls = append(f.calls, "managed-taps")
	return nil
}
func (f *fakeLinuxNetworkOps) EnsureBridge(name string) error {
	f.calls = append(f.calls, "bridge:"+name)
	return nil
}
func (f *fakeLinuxNetworkOps) EnsureBridgeSTP(name string, enabled bool) error {
	f.calls = append(f.calls, "stp:"+name+"="+strconv.FormatBool(enabled))
	return nil
}
func (f *fakeLinuxNetworkOps) EnsureBridgeAddress(name, cidr string) error {
	f.calls = append(f.calls, "addr:"+name+"="+cidr)
	return nil
}
func (f *fakeLinuxNetworkOps) EnsureLinkUp(name string) error {
	f.calls = append(f.calls, "up:"+name)
	return nil
}
func (f *fakeLinuxNetworkOps) CreateTap(name, bridge string) (*os.File, error) {
	f.calls = append(f.calls, "tap:"+name+"->"+bridge)
	if f.tap != nil {
		return f.tap, nil
	}
	return os.OpenFile(filepath.Clean(os.DevNull), os.O_RDWR, 0)
}

type fakeLinuxNFTConverger struct {
	subnetIndexes []int
	calls         int
}

func (f *fakeLinuxNFTConverger) Converge(subnetIndexes []int) error {
	f.calls++
	f.subnetIndexes = append([]int(nil), subnetIndexes...)
	return nil
}

func mustCIDR(value string) net.Addr {
	ip, network, err := net.ParseCIDR(value)
	if err != nil {
		panic(err)
	}
	network.IP = ip
	return network
}

func mustRoute(name, cidr string) cluster.HostRoute {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return cluster.HostRoute{Interface: name, Network: network}
}
