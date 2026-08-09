//go:build linux && e2e

package helper

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/nftables"
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
	if chainCount != 2 || ruleCount != 3 {
		t.Fatalf("owned nftables state = %d chains/%d rules, want 2/3", chainCount, ruleCount)
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

func TestLinuxNetworkingWithCapabilitySet(t *testing.T) {
	requireLinuxHelperE2E(t)
	const childEnv = "TBX_LINUX_CAPSH_CHILD"
	const subnetIndex = 250
	if os.Getenv(childEnv) == "1" {
		if os.Geteuid() == 0 {
			t.Fatal("capability probe still runs as root")
		}
		assertLinuxCapabilitySet(t)
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
