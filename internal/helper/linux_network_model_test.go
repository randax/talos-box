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
			{Name: bridgeNameForSubnet(4), Alias: bridgeAliasForSubnet(4), Addrs: []net.Addr{mustCIDR("172.30.4.1/24")}},
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
		defaultRoute: mustRoute("eth0", "0.0.0.0/0"),
	}
	nft := &fakeLinuxNFTConverger{}

	_, err := startLinuxAttachment(ops, nft, []int{4, 7}, 7, "demo", "cp-1")
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

	_, err := startLinuxAttachment(ops, &fakeLinuxNFTConverger{}, []int{7}, 7, "demo", "cp-1")
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
			{Name: bridgeNameForSubnet(2), Alias: bridgeAliasForSubnet(2), Addrs: []net.Addr{mustCIDR("172.30.2.1/24")}},
		},
		defaultRoute: mustRoute("en0", "0.0.0.0/0"),
		tap:          tap,
	}
	nft := &fakeLinuxNFTConverger{}

	got, err := startLinuxAttachment(ops, nft, []int{2, 3}, 3, "demo", "cp-1")
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
			{Name: bridgeNameForSubnet(7), Alias: bridgeAliasForSubnet(7), Addrs: []net.Addr{mustCIDR("172.30.7.1/24")}},
			{Name: "eth0", Addrs: []net.Addr{mustCIDR("10.0.0.5/24")}},
			{Name: bridgeNameForSubnet(3), Alias: bridgeAliasForSubnet(3), Addrs: []net.Addr{mustCIDR("172.30.3.1/24")}},
		},
		defaultRoute: mustRoute("eth0", "0.0.0.0/0"),
	}
	nft := &fakeLinuxNFTConverger{}

	if err := convergeLinuxManagedState(ops, nft, []int{7, 3}); err != nil {
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
		"managed-taps:3,7",
	}
	if !reflect.DeepEqual(ops.calls, wantCalls) {
		t.Fatalf("network calls = %#v, want %#v", ops.calls, wantCalls)
	}
	if got := nft.subnetIndexes; !reflect.DeepEqual(got, []int{3, 7}) {
		t.Fatalf("nft subnet indexes = %v, want [3 7]", got)
	}
}

func TestConvergeLinuxManagedStateRecreatesBridgeForConfiguredSubnet(t *testing.T) {
	t.Parallel()

	ops := &fakeLinuxNetworkOps{
		links:        []linuxLinkState{{Name: tapNameForNode(12, "demo", "cp-1"), Alias: tapAliasForSubnet(12)}},
		defaultRoute: mustRoute("eth0", "0.0.0.0/0"),
	}
	nft := &fakeLinuxNFTConverger{}
	if err := convergeLinuxManagedState(ops, nft, []int{12}); err != nil {
		t.Fatal(err)
	}
	if got, want := ops.calls, []string{
		"ipv4-forwarding",
		"bridge:" + bridgeNameForSubnet(12),
		"stp:" + bridgeNameForSubnet(12) + "=false",
		"addr:" + bridgeNameForSubnet(12) + "=172.30.12.1/24",
		"up:" + bridgeNameForSubnet(12),
		"iface-forwarding:" + bridgeNameForSubnet(12),
		"managed-taps:12",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("network calls = %#v, want %#v", got, want)
	}
	if got := nft.subnetIndexes; !reflect.DeepEqual(got, []int{12}) {
		t.Fatalf("nft subnet indexes = %v, want [12]", got)
	}
}

func TestConvergeLinuxManagedStateRecreatesColdBootBridgesFromInventory(t *testing.T) {
	t.Parallel()

	ops := &fakeLinuxNetworkOps{defaultRoute: mustRoute("eth0", "0.0.0.0/0")}
	nft := &fakeLinuxNFTConverger{}
	if err := convergeLinuxManagedState(ops, nft, []int{14}); err != nil {
		t.Fatal(err)
	}
	if got, want := ops.calls, []string{
		"ipv4-forwarding",
		"bridge:" + bridgeNameForSubnet(14),
		"stp:" + bridgeNameForSubnet(14) + "=false",
		"addr:" + bridgeNameForSubnet(14) + "=172.30.14.1/24",
		"up:" + bridgeNameForSubnet(14),
		"iface-forwarding:" + bridgeNameForSubnet(14),
		"managed-taps:14",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("network calls = %#v, want %#v", got, want)
	}
	if got := nft.subnetIndexes; !reflect.DeepEqual(got, []int{14}) {
		t.Fatalf("nft subnet indexes = %v, want [14]", got)
	}
}

func TestConvergeLinuxManagedStateSkipsACapturedSubnetAndConvergesTheRest(t *testing.T) {
	t.Parallel()

	ops := &fakeLinuxNetworkOps{routes: map[string]cluster.HostRoute{
		"172.30.0.2": mustRoute("eth0", "0.0.0.0/0"),
		"172.30.1.2": mustRoute("eth0", "0.0.0.0/0"),
		"172.30.7.2": mustRoute("vpn0", "172.30.6.0/23"),
	}}
	nft := &fakeLinuxNFTConverger{}
	err := convergeLinuxManagedState(ops, nft, []int{0, 7})
	if err == nil {
		t.Fatal("convergeLinuxManagedState() error = nil, want collision")
	}
	for _, fragment := range []string{"vpn0", "try subnet index 1"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error = %q, want %q", err, fragment)
		}
	}
	var preflight *SubnetPreflightError
	if !errors.As(err, &preflight) || !reflect.DeepEqual(preflight.Skipped, []int{7}) {
		t.Fatalf("error = %#v, want a SubnetPreflightError skipping [7]", err)
	}
	if got := nft.subnetIndexes; !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("nft subnet indexes = %v, want [0]: the captured subnet must not stop the others", got)
	}
	for _, call := range ops.calls {
		if strings.Contains(call, bridgeNameForSubnet(7)) {
			t.Fatalf("captured subnet was mutated: %v", ops.calls)
		}
	}
}

func TestManagedSubnetIndexesRequireOwnershipAlias(t *testing.T) {
	t.Parallel()

	links := []linuxLinkState{
		{Name: bridgeNameForSubnet(3)},
		{Name: bridgeNameForSubnet(4), Alias: bridgeAliasForSubnet(4)},
		{Name: tapNameForNode(5, "demo", "cp-1")},
		{Name: tapNameForNode(6, "demo", "cp-1"), Alias: tapAliasForSubnet(6)},
	}
	if got, want := managedSubnetIndexes(links), []int{4, 6}; !reflect.DeepEqual(got, want) {
		t.Fatalf("managedSubnetIndexes() = %v, want %v", got, want)
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
			return []linuxLinkState{{Name: bridgeNameForSubnet(9), Alias: bridgeAliasForSubnet(9), Addrs: []net.Addr{mustCIDR("172.30.9.1/24")}}}, nil
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

func TestTeardownLinuxBridgeDeletesTheSubnetBridge(t *testing.T) {
	t.Parallel()

	ops := &fakeLinuxNetworkOps{}
	removed, err := teardownLinuxBridge(ops, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("teardownLinuxBridge() removed = false, want true")
	}
	if want := []string{"delete-bridge:" + bridgeNameForSubnet(3)}; !reflect.DeepEqual(ops.calls, want) {
		t.Fatalf("network calls = %#v, want %#v", ops.calls, want)
	}
}

func TestTeardownLinuxBridgeIsIdempotent(t *testing.T) {
	t.Parallel()

	ops := &fakeLinuxNetworkOps{deleteAbsent: true}
	removed, err := teardownLinuxBridge(ops, 0)
	if err != nil {
		t.Fatalf("teardownLinuxBridge() error = %v, want nil for an absent bridge", err)
	}
	if removed {
		t.Fatal("teardownLinuxBridge() removed = true, want false for an absent bridge")
	}
}

func TestTeardownLinuxBridgeRejectsOutOfRangeSubnet(t *testing.T) {
	t.Parallel()

	for _, index := range []int{-1, cluster.MaxSubnetIndex + 1} {
		ops := &fakeLinuxNetworkOps{}
		if _, err := teardownLinuxBridge(ops, index); err == nil {
			t.Fatalf("teardownLinuxBridge(%d) error = nil, want out-of-range refusal", index)
		}
		if len(ops.calls) != 0 {
			t.Fatalf("network operations were attempted for subnet %d: %v", index, ops.calls)
		}
	}
}

type fakeLinuxNetworkOps struct {
	links        []linuxLinkState
	routes       map[string]cluster.HostRoute
	defaultRoute cluster.HostRoute
	tap          *os.File
	calls        []string
	deleteAbsent bool
	deleteErr    error
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
func (f *fakeLinuxNetworkOps) EnsureManagedTaps(indexes []int) error {
	values := make([]string, len(indexes))
	for index, value := range indexes {
		values[index] = strconv.Itoa(value)
	}
	f.calls = append(f.calls, "managed-taps:"+strings.Join(values, ","))
	return nil
}
func (f *fakeLinuxNetworkOps) EnsureBridge(name string) error {
	f.calls = append(f.calls, "bridge:"+name)
	return nil
}
func (f *fakeLinuxNetworkOps) DeleteBridge(name string) (bool, error) {
	f.calls = append(f.calls, "delete-bridge:"+name)
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	return !f.deleteAbsent, nil
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

func TestConvergeLinuxManagedStateKeepsAnOwnedBridgeOnACapturedSubnet(t *testing.T) {
	t.Parallel()

	ops := &fakeLinuxNetworkOps{
		links: []linuxLinkState{{Name: bridgeNameForSubnet(7), Alias: bridgeAliasForSubnet(7)}},
		routes: map[string]cluster.HostRoute{
			"172.30.0.2": mustRoute("eth0", "0.0.0.0/0"),
			"172.30.7.2": mustRoute("vpn0", "172.30.6.0/23"),
		},
	}
	nft := &fakeLinuxNFTConverger{}
	if err := convergeLinuxManagedState(ops, nft, []int{0, 7}); err != nil {
		t.Fatalf("convergeLinuxManagedState() error = %v, want the owned subnet kept without complaint", err)
	}
	if got := nft.subnetIndexes; !reflect.DeepEqual(got, []int{0, 7}) {
		t.Fatalf("nft subnet indexes = %v, want [0 7]: a running cluster keeps its rules", got)
	}
}
