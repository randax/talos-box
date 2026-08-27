//go:build linux && e2e

package helper

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/nftables"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/vishvananda/netlink"
)

const linuxHelperE2EEnv = "TBX_LINUX_HELPER_E2E"

const linuxCapabilityMask uint64 = 1<<10 | 1<<12 | 1<<13

func requireLinuxHelperE2E(t *testing.T) {
	t.Helper()
	if os.Getenv(linuxHelperE2EEnv) != "1" {
		t.Skip(linuxHelperE2EEnv + "=1 is required; this test mutates its Linux network namespace")
	}
}

func TestLinuxNetworkingConvergesIdempotently(t *testing.T) {
	requireLinuxHelperE2E(t)
	const subnetIndex = 248
	bridgeName := bridgeNameForSubnet(subnetIndex)
	t.Cleanup(func() { removeLinuxE2EBridge(bridgeName) })
	t.Cleanup(removeLinuxE2ETable)
	persistLinuxE2ECluster(t, "e2e", subnetIndex)

	if err := ConvergeNetworking(); err != nil {
		t.Fatalf("cold-boot convergence: %v", err)
	}

	attachment, err := StartInterface(subnetIndex, "e2e", "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = attachment.close() })
	if attachment.Kind != AttachmentTapFD {
		t.Fatalf("attachment kind = %q, want %q", attachment.Kind, AttachmentTapFD)
	}

	if err := ConvergeNetworking(); err != nil {
		t.Fatalf("first restart convergence: %v", err)
	}
	if err := ConvergeNetworking(); err != nil {
		t.Fatalf("second restart convergence: %v", err)
	}

	bridge, err := requireLinuxBridge(bridgeName)
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := netlink.AddrList(bridge, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 1 || !addresses[0].IP.Equal(net.IPv4(172, 30, subnetIndex, 1)) {
		t.Fatalf("bridge addresses = %v, want 172.30.%d.1/24", addresses, subnetIndex)
	}
	stp, err := os.ReadFile("/sys/class/net/" + bridgeName + "/bridge/stp_state")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stp)) != "0" {
		t.Fatalf("bridge STP state = %q, want 0", strings.TrimSpace(string(stp)))
	}
	if bridge.Attrs().Alias != bridgeAliasForSubnet(subnetIndex) {
		t.Fatalf("bridge alias = %q, want %q", bridge.Attrs().Alias, bridgeAliasForSubnet(subnetIndex))
	}
	for _, path := range []string{
		"/proc/sys/net/ipv4/ip_forward",
		"/proc/sys/net/ipv4/conf/" + bridgeName + "/forwarding",
	} {
		value, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(value)) != "1" {
			t.Fatalf("%s = %q, want 1", path, strings.TrimSpace(string(value)))
		}
	}
	tap, err := netlink.LinkByName(tapNameForNode(subnetIndex, "e2e", "cp-1"))
	if err != nil {
		t.Fatal(err)
	}
	if tap.Attrs().MasterIndex != bridge.Attrs().Index {
		t.Fatalf("tap master = %d, want bridge index %d", tap.Attrs().MasterIndex, bridge.Attrs().Index)
	}
	if tap.Attrs().Alias != tapAliasForSubnet(subnetIndex) {
		t.Fatalf("tap alias = %q, want %q", tap.Attrs().Alias, tapAliasForSubnet(subnetIndex))
	}

	connection := &nftables.Conn{}
	tables, err := connection.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, table := range tables {
		if table.Name == linuxNFTTableName {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("table inet %s count = %d, want exactly 1", linuxNFTTableName, count)
	}
	table, err := connection.ListTableOfFamily(linuxNFTTableName, nftables.TableFamilyINet)
	if err != nil {
		t.Fatal(err)
	}
	chains, err := connection.ListChainsOfTableFamily(nftables.TableFamilyINet)
	if err != nil {
		t.Fatal(err)
	}
	chainCount, ruleCount := 0, 0
	for _, chain := range chains {
		if chain.Table.Name != table.Name {
			continue
		}
		chainCount++
		rules, err := connection.GetRules(table, chain)
		if err != nil {
			t.Fatal(err)
		}
		ruleCount += len(rules)
	}
	if chainCount != 3 || ruleCount != 3 {
		t.Fatalf("owned nftables state = %d chains/%d rules, want 3/3", chainCount, ruleCount)
	}
}

func TestLinuxNetworkingRefusesForeignNFTTable(t *testing.T) {
	requireLinuxHelperE2E(t)
	t.Cleanup(removeLinuxE2ETable)

	connection := &nftables.Conn{}
	table := connection.AddTable(&nftables.Table{Name: linuxNFTTableName, Family: nftables.TableFamilyINet})
	connection.AddChain(&nftables.Chain{Name: "foreign-policy", Table: table})
	if err := connection.Flush(); err != nil {
		t.Fatal(err)
	}

	err := (realLinuxNFTConverger{}).Converge([]int{247})
	if err == nil || !strings.Contains(err.Error(), "without Talos Box ownership marker") {
		t.Fatalf("Converge() error = %v, want ownership refusal", err)
	}
	chains, err := connection.ListChainsOfTableFamily(nftables.TableFamilyINet)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, chain := range chains {
		if chain.Table.Name == linuxNFTTableName && chain.Name == "foreign-policy" {
			found = true
		}
	}
	if !found {
		t.Fatal("foreign nftables table was modified after ownership refusal")
	}
}

func TestLinuxNetworkingRefusesForeignLookalikeBridge(t *testing.T) {
	requireLinuxHelperE2E(t)
	const subnetIndex = 247
	name := bridgeNameForSubnet(subnetIndex)
	foreign := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeLinuxE2EBridge(name) })

	err := (realLinuxNetworkOps{}).EnsureBridge(name)
	if err == nil || !strings.Contains(err.Error(), "not owned by Talos Box") {
		t.Fatalf("EnsureBridge() error = %v, want ownership refusal", err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if link.Attrs().Alias != "" {
		t.Fatalf("foreign bridge alias changed to %q", link.Attrs().Alias)
	}
}

func TestLinuxNetworkingRejectsVPNCollisionWithFreeIndex(t *testing.T) {
	requireLinuxHelperE2E(t)
	const subnetIndex = 249
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "tbx-vpn-e2e"}}
	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(dummy) })
	address, err := netlink.ParseAddr("172.30.249.10/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(dummy, address); err != nil {
		t.Fatal(err)
	}

	_, err = StartInterface(subnetIndex, "e2e", "cp-2")
	if err == nil {
		t.Fatal("StartInterface() error = nil, want collision")
	}
	for _, fragment := range []string{"172.30.249.0/24", "tbx-vpn-e2e", "try subnet index"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error = %q, want %q", err, fragment)
		}
	}
	if _, lookupErr := netlink.LinkByName(bridgeNameForSubnet(subnetIndex)); lookupErr == nil {
		t.Fatalf("bridge %s was created after collision", bridgeNameForSubnet(subnetIndex))
	}
}

func TestLinuxNetworkingDetectsVPNRouteBehindOwnedBridgeRoute(t *testing.T) {
	requireLinuxHelperE2E(t)
	const subnetIndex = 246
	bridgeName := bridgeNameForSubnet(subnetIndex)
	t.Cleanup(func() { removeLinuxE2EBridge(bridgeName) })
	t.Cleanup(removeLinuxE2ETable)
	persistLinuxE2ECluster(t, "route-e2e", subnetIndex)
	if err := ConvergeNetworking(); err != nil {
		t.Fatal(err)
	}

	vpn := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "tbx-vpn-route"}}
	if err := netlink.LinkAdd(vpn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(vpn) })
	if err := netlink.LinkSetUp(vpn); err != nil {
		t.Fatal(err)
	}
	_, network, err := net.ParseCIDR("172.30.246.0/23")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: vpn.Attrs().Index, Dst: network}); err != nil {
		t.Fatal(err)
	}

	_, err = StartInterface(subnetIndex, "route-e2e", "cp-1")
	if err == nil {
		t.Fatal("StartInterface() error = nil, want VPN route collision")
	}
	for _, fragment := range []string{"172.30.246.0/24", "tbx-vpn-route", "try subnet index"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error = %q, want %q", err, fragment)
		}
	}
}

func TestLinuxNetworkingWithCapabilitySet(t *testing.T) {
	requireLinuxHelperE2E(t)
	const childEnv = "TBX_LINUX_CAPSH_CHILD"
	const subnetIndex = 250
	if os.Getenv(childEnv) == "1" {
		if os.Geteuid() == 0 {
			t.Fatal("capability probe still runs as root")
		}
		assertLinuxCapabilitySet(t)
		persistLinuxE2ECluster(t, "cap-e2e", subnetIndex)
		t.Cleanup(func() { removeLinuxE2EBridge(bridgeNameForSubnet(subnetIndex)) })
		t.Cleanup(removeLinuxE2ETable)
		attachment, err := StartInterface(subnetIndex, "cap-e2e", "cp-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := attachment.close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("root is required to construct the restricted capsh child")
	}
	capsh, err := exec.LookPath("capsh")
	if err != nil {
		t.Skip("capsh is not installed")
	}
	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	capabilities := "cap_net_admin,cap_net_bind_service,cap_net_raw"
	command := exec.Command(
		capsh,
		"--keep=1",
		"--user=nobody",
		"--caps="+capabilities+"+eip",
		"--addamb=cap_net_admin",
		"--addamb=cap_net_bind_service",
		"--addamb=cap_net_raw",
		"--",
		"-c",
		`exec "$TBX_CAP_TEST_BINARY" -test.run '^TestLinuxNetworkingWithCapabilitySet$' -test.v`,
	)
	command.Env = append(os.Environ(), childEnv+"=1", "TBX_CAP_TEST_BINARY="+binary)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("capsh capability probe failed: %v\n%s", err, output)
	}
}

func TestLinuxWorldConnectableSocketAuthorizesPeerUID(t *testing.T) {
	requireLinuxHelperE2E(t)
	const childEnv = "TBX_LINUX_SOCKET_CLIENT_CHILD"
	const socketEnv = "TBX_LINUX_SOCKET_CLIENT_PATH"
	if os.Getenv(childEnv) == "1" {
		address, err := net.ResolveUnixAddr("unix", os.Getenv(socketEnv))
		if err != nil {
			t.Fatal(err)
		}
		connection, err := net.DialUnix("unix", nil, address)
		if err != nil {
			t.Fatalf("unprivileged dial: %v", err)
		}
		client := &Client{connection: connection}
		defer func() { _ = client.Close() }()
		if err := client.Ping(); err != nil {
			t.Fatalf("authorized unprivileged ping: %v", err)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("root is required to launch a different-UID client")
	}
	capsh, err := exec.LookPath("capsh")
	if err != nil {
		t.Skip("capsh is not installed")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal(err)
	}
	parsedUID, err := strconv.ParseUint(nobody.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	allowedUID := uint32(parsedUID)
	socketPath := fmt.Sprintf("/tmp/tbx-helper-auth-%d.sock", os.Getpid())
	listener, err := Listen(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, &allowedUID)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown()
		<-done
		_ = os.Remove(socketPath)
	})

	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		capsh,
		"--user=nobody",
		"--",
		"-c",
		`exec "$TBX_CAP_TEST_BINARY" -test.run '^TestLinuxWorldConnectableSocketAuthorizesPeerUID$' -test.v`,
	)
	command.Env = append(os.Environ(), childEnv+"=1", socketEnv+"="+socketPath, "TBX_CAP_TEST_BINARY="+binary)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged socket client failed: %v\n%s", err, output)
	}
}

func persistLinuxE2ECluster(t *testing.T, name string, subnetIndex int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New(name, subnetIndex, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
}

func assertLinuxCapabilitySet(t *testing.T) {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]uint64)
	for _, line := range strings.Split(string(status), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || (name != "CapInh" && name != "CapPrm" && name != "CapEff" && name != "CapAmb") {
			continue
		}
		parsed, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		values[name] = parsed
	}
	for _, name := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb"} {
		if values[name] != linuxCapabilityMask {
			t.Fatalf("%s = %#x, want only NET_BIND_SERVICE, NET_ADMIN, and NET_RAW (%#x)", name, values[name], linuxCapabilityMask)
		}
	}
}

func removeLinuxE2EBridge(name string) {
	link, err := netlink.LinkByName(name)
	if err == nil {
		_ = netlink.LinkDel(link)
	}
}

func removeLinuxE2ETable() {
	connection := &nftables.Conn{}
	tables, err := connection.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return
	}
	for _, table := range tables {
		if table.Name == linuxNFTTableName {
			connection.DelTable(table)
		}
	}
	_ = connection.Flush()
}
