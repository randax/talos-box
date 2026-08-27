//go:build linux

package helper

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

var (
	linuxNetwork linuxNetworkOps   = realLinuxNetworkOps{}
	linuxNFT     linuxNFTConverger = realLinuxNFTConverger{}
)

// StartInterface converges the cluster's host networking and returns a tap
// descriptor for QEMU. The helper retains its copy until Detach.
func StartInterface(configured []int, subnetIndex int, clusterName, node string) (*platformAttachment, error) {
	file, err := startLinuxAttachment(linuxNetwork, linuxNFT, configured, subnetIndex, clusterName, node)
	if err != nil {
		return nil, err
	}
	return &platformAttachment{
		Kind: AttachmentTapFD,
		FD:   int(file.Fd()),
		stop: file.Close,
	}, nil
}

// TeardownSubnet removes the host bridge for a subnet no cluster owns any more,
// taking its gateway address with it. It reports whether a bridge was removed,
// so a caller can tell residue from a host that never had one.
func TeardownSubnet(subnetIndex int) (bool, error) {
	return teardownLinuxBridge(linuxNetwork, subnetIndex)
}

func convergeNetworking(subnets []int) error {
	return convergeLinuxManagedState(linuxNetwork, linuxNFT, subnets)
}

func enableForwarding() error {
	return linuxNetwork.EnsureIPv4Forwarding()
}

type realLinuxNetworkOps struct{}

func (realLinuxNetworkOps) ListLinks() ([]linuxLinkState, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("dump netlink links: %w", err)
	}
	return listLinuxLinkStates(links, netlink.AddrList, netlink.LinkByIndex)
}

func listLinuxLinkStates(
	links []netlink.Link,
	addrList func(netlink.Link, int) ([]netlink.Addr, error),
	linkByIndex func(int) (netlink.Link, error),
) ([]linuxLinkState, error) {
	result := make([]linuxLinkState, 0, len(links))
	for _, link := range links {
		addresses, err := addrList(link, netlink.FAMILY_V4)
		if err != nil {
			_, lookupErr := linkByIndex(link.Attrs().Index)
			var notFound netlink.LinkNotFoundError
			if errors.As(lookupErr, &notFound) {
				continue
			}
			if lookupErr != nil {
				return nil, fmt.Errorf("inspect link metadata for %s: %w", link.Attrs().Name, lookupErr)
			}
			return nil, fmt.Errorf("dump netlink addresses for %s: %w", link.Attrs().Name, err)
		}
		current := linuxLinkState{Name: link.Attrs().Name, Alias: link.Attrs().Alias, Addrs: make([]net.Addr, 0, len(addresses))}
		for _, address := range addresses {
			if address.IPNet != nil {
				current.Addrs = append(current.Addrs, address.IPNet)
			}
		}
		result = append(result, current)
	}
	return result, nil
}

func (realLinuxNetworkOps) Route(destination net.IP) (cluster.HostRoute, error) {
	return cluster.SystemSubnetSources().Route(destination)
}

func (realLinuxNetworkOps) EnsureIPv4Forwarding() error {
	return ensureLinuxSysctl("/proc/sys/net/ipv4/ip_forward")
}

func (realLinuxNetworkOps) EnsureInterfaceForwarding(name string) error {
	if _, ok := subnetIndexFromBridgeName(name); !ok {
		return fmt.Errorf("refuse forwarding sysctl for unmanaged interface %q", name)
	}
	return ensureLinuxSysctl(filepath.Join("/proc/sys/net/ipv4/conf", name, "forwarding"))
}

func (realLinuxNetworkOps) EnsureManagedTaps(subnetIndexes []int) error {
	desired := make(map[int]struct{}, len(subnetIndexes))
	for _, index := range subnetIndexes {
		desired[index] = struct{}{}
	}
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("dump links while converging taps: %w", err)
	}
	for _, link := range links {
		index, ok := subnetIndexFromTapName(link.Attrs().Name)
		if !ok {
			continue
		}
		if link.Attrs().Alias != tapAliasForSubnet(index) {
			continue
		}
		if _, ok := desired[index]; !ok {
			continue
		}
		tap, ok := link.(*netlink.Tuntap)
		if !ok || tap.Mode != netlink.TUNTAP_MODE_TAP {
			return fmt.Errorf("helper-owned interface name %s has type %s, want tap", link.Attrs().Name, link.Type())
		}
		flags, err := readLinuxTunFlags(tap.Attrs().Name)
		if err != nil {
			return fmt.Errorf("inspect tap framing for %s: %w", tap.Attrs().Name, err)
		}
		if err := requireLinuxTapNoPacketInfo(tap.Attrs().Name, flags); err != nil {
			return err
		}
		bridge, err := requireLinuxBridge(bridgeNameForSubnet(index))
		if err != nil {
			return err
		}
		if err := netlink.LinkSetMaster(tap, bridge); err != nil {
			return fmt.Errorf("enslave tap %s to %s: %w", tap.Attrs().Name, bridge.Attrs().Name, err)
		}
		if err := netlink.LinkSetUp(tap); err != nil {
			return fmt.Errorf("bring tap %s up: %w", tap.Attrs().Name, err)
		}
	}
	return nil
}

func requireLinuxTapNoPacketInfo(name string, flags int64) error {
	if flags&unix.IFF_NO_PI == 0 {
		return fmt.Errorf("helper-owned tap %s uses packet-info framing; stop the stale VM before retrying", name)
	}
	return nil
}

func readLinuxTunFlags(name string) (int64, error) {
	contents, err := os.ReadFile(filepath.Join("/sys/class/net", name, "tun_flags"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(contents)), 0, 64)
}

func ensureLinuxSysctl(path string) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if strings.TrimSpace(string(current)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1\n"), 0); err != nil {
		if errors.Is(err, os.ErrPermission) {
			// The packaged helper runs without CAP_DAC_OVERRIDE, so a sysctl
			// the host has not already set is out of its reach: the package's
			// sysctl.d drop-in (applied by the unit's ExecStartPre) is the fix.
			return fmt.Errorf("enable IPv4 forwarding in %s: %w; set net.ipv4.ip_forward=1 on the host (install packaging/linux/usr/lib/sysctl.d/50-talos-box.conf and run `sudo systemctl restart tbx-helper.service`)", path, err)
		}
		return fmt.Errorf("enable IPv4 forwarding in %s: %w", path, err)
	}
	return nil
}

func (realLinuxNetworkOps) EnsureBridge(name string) error {
	link, err := netlink.LinkByName(name)
	created := false
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("inspect bridge %s: %w", name, err)
		}
		index, ok := subnetIndexFromBridgeName(name)
		if !ok {
			return fmt.Errorf("refuse to create unmanaged bridge %q", name)
		}
		if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
			return fmt.Errorf("create bridge %s: %w", name, err)
		}
		created = true
		link, err = netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("inspect created bridge %s: %w", name, err)
		}
		if err := netlink.LinkSetAlias(link, bridgeAliasForSubnet(index)); err != nil {
			_ = netlink.LinkDel(link)
			return fmt.Errorf("mark bridge %s as Talos Box-owned: %w", name, err)
		}
		markedLink := link
		link, err = netlink.LinkByName(name)
		if err != nil {
			_ = netlink.LinkDel(markedLink)
			return fmt.Errorf("verify bridge %s ownership: %w", name, err)
		}
	}
	if _, ok := link.(*netlink.Bridge); !ok {
		return fmt.Errorf("interface %s already exists with type %s, want bridge", name, link.Type())
	}
	index, ok := subnetIndexFromBridgeName(name)
	if !ok || link.Attrs().Alias != bridgeAliasForSubnet(index) {
		if created {
			_ = netlink.LinkDel(link)
		}
		return fmt.Errorf("interface %s is not owned by Talos Box; refusing to modify it", name)
	}
	return nil
}

func (realLinuxNetworkOps) DeleteBridge(name string) (bool, error) {
	// A single lookup keeps absent-is-success race-free: requireLinuxBridge
	// wraps the netlink error, so a bridge that is already gone surfaces here
	// rather than failing a second lookup.
	bridge, err := requireLinuxBridge(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, err
	}
	links, err := netlink.LinkList()
	if err != nil {
		return false, fmt.Errorf("dump links while removing bridge %s: %w", name, err)
	}
	// Subnets are allocated one cluster at a time, so a tap still enslaved here
	// belongs to a VM that outlived its cluster state; deleting the bridge under
	// it would cut a live guest's only link.
	for _, link := range links {
		if link.Attrs().MasterIndex == bridge.Attrs().Index {
			return false, fmt.Errorf("bridge %s still has %s attached; stop the VM before removing it", name, link.Attrs().Name)
		}
	}
	if err := netlink.LinkDel(bridge); err != nil {
		return false, fmt.Errorf("delete bridge %s: %w", name, err)
	}
	return true, nil
}

func (realLinuxNetworkOps) EnsureBridgeSTP(name string, enabled bool) error {
	link, err := requireLinuxBridge(name)
	if err != nil {
		return err
	}
	request := nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)
	message := nl.NewIfInfomsg(unix.AF_UNSPEC)
	message.Index = int32(link.Attrs().Index)
	request.AddData(message)
	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated("bridge"))
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	state := uint32(0)
	if enabled {
		state = 1
	}
	data.AddRtAttr(nl.IFLA_BR_STP_STATE, nl.Uint32Attr(state))
	request.AddData(linkInfo)
	if _, err := request.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("set bridge %s STP state to %d: %w", name, state, err)
	}
	return nil
}

func (realLinuxNetworkOps) EnsureBridgeAddress(name, cidr string) error {
	link, err := requireLinuxBridge(name)
	if err != nil {
		return err
	}
	desired, err := netlink.ParseAddr(cidr)
	if err != nil {
		return fmt.Errorf("parse bridge address %s: %w", cidr, err)
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("dump bridge %s addresses: %w", name, err)
	}
	found := false
	for _, address := range addresses {
		if address.IPNet != nil && address.IP.Equal(desired.IP) && bytes.Equal(address.Mask, desired.Mask) {
			found = true
			continue
		}
		return fmt.Errorf("bridge %s has foreign IPv4 address %s; refusing to replace it", name, address.String())
	}
	if found {
		return nil
	}
	if err := netlink.AddrReplace(link, desired); err != nil {
		return fmt.Errorf("set bridge %s address %s: %w", name, cidr, err)
	}
	return nil
}

func (realLinuxNetworkOps) EnsureLinkUp(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("inspect link %s: %w", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring link %s up: %w", name, err)
	}
	return nil
}

func newLinuxTap(name string, subnetIndex, masterIndex int) *netlink.Tuntap {
	return &netlink.Tuntap{
		LinkAttrs:  netlink.LinkAttrs{Name: name, Alias: tapAliasForSubnet(subnetIndex), MasterIndex: masterIndex},
		Mode:       netlink.TUNTAP_MODE_TAP,
		Flags:      netlink.TUNTAP_DEFAULTS | netlink.TUNTAP_NO_PI,
		NonPersist: true,
		Queues:     1,
		Owner:      uint32(os.Geteuid()),
		Group:      uint32(os.Getegid()),
	}
}

func (realLinuxNetworkOps) CreateTap(name, bridgeName string) (*os.File, error) {
	subnetIndex, ok := subnetIndexFromTapName(name)
	if !ok || bridgeName != bridgeNameForSubnet(subnetIndex) {
		return nil, fmt.Errorf("refuse to create unmanaged tap %q for bridge %q", name, bridgeName)
	}
	bridge, err := requireLinuxBridge(bridgeName)
	if err != nil {
		return nil, err
	}
	if existing, lookupErr := netlink.LinkByName(name); lookupErr == nil {
		return nil, fmt.Errorf("tap %s already exists as %s; stop the stale VM before retrying", name, existing.Type())
	} else {
		var notFound netlink.LinkNotFoundError
		if !errors.As(lookupErr, &notFound) {
			return nil, fmt.Errorf("inspect tap %s: %w", name, lookupErr)
		}
	}

	tap := newLinuxTap(name, subnetIndex, bridge.Attrs().Index)
	if err := netlink.LinkAdd(tap); err != nil {
		return nil, fmt.Errorf("create tap %s: %w", name, err)
	}
	closeTap := func() {
		for _, file := range tap.Fds {
			_ = file.Close()
		}
	}
	if len(tap.Fds) != 1 {
		closeTap()
		return nil, fmt.Errorf("create tap %s returned %d descriptors, want 1", name, len(tap.Fds))
	}
	if err := netlink.LinkSetAlias(tap, tapAliasForSubnet(subnetIndex)); err != nil {
		closeTap()
		return nil, fmt.Errorf("mark tap %s as Talos Box-owned: %w", name, err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		closeTap()
		return nil, fmt.Errorf("bring tap %s up: %w", name, err)
	}
	return tap.Fds[0], nil
}

func requireLinuxBridge(name string) (*netlink.Bridge, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("inspect bridge %s: %w", name, err)
	}
	bridge, ok := link.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("interface %s has type %s, want bridge", name, link.Type())
	}
	index, ok := subnetIndexFromBridgeName(name)
	if !ok || bridge.Attrs().Alias != bridgeAliasForSubnet(index) {
		return nil, fmt.Errorf("interface %s is not owned by Talos Box; refusing to modify it", name)
	}
	return bridge, nil
}

type realLinuxNFTConverger struct{}

func (realLinuxNFTConverger) Converge(subnetIndexes []int) error {
	connection, err := nftables.New()
	if err != nil {
		return fmt.Errorf("connect to nftables: %w", err)
	}
	plan := buildLinuxNFTPlan(subnetIndexes)
	tables, err := connection.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return fmt.Errorf("list inet nftables tables: %w", err)
	}
	for _, table := range tables {
		if table.Name == plan.tableName {
			owned, err := linuxNFTTableOwned(connection, table)
			if err != nil {
				return err
			}
			if !owned {
				return fmt.Errorf("table inet %s already exists without Talos Box ownership marker; refusing to replace it", plan.tableName)
			}
			connection.DelTable(table)
		}
	}

	table := connection.AddTable(&nftables.Table{Name: plan.tableName, Family: nftables.TableFamilyINet})
	connection.AddChain(&nftables.Chain{Name: linuxNFTOwnerMarkerChain, Table: table})
	accept := nftables.ChainPolicyAccept
	forward := connection.AddChain(&nftables.Chain{
		Name:     "forward",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &accept,
	})
	postrouting := connection.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
		Policy:   &accept,
	})
	chains := map[string]*nftables.Chain{"forward": forward, "postrouting": postrouting}
	for _, rule := range plan.rules {
		expressions, err := linuxNFTExpressions(rule)
		if err != nil {
			return err
		}
		connection.AddRule(&nftables.Rule{Table: table, Chain: chains[rule.chain], Exprs: expressions})
	}
	if err := connection.Flush(); err != nil {
		return fmt.Errorf("replace table inet %s: %w", plan.tableName, err)
	}
	return nil
}

func linuxNFTTableOwned(connection *nftables.Conn, table *nftables.Table) (bool, error) {
	chains, err := connection.ListChainsOfTableFamily(nftables.TableFamilyINet)
	if err != nil {
		return false, fmt.Errorf("inspect ownership of table inet %s: %w", table.Name, err)
	}
	for _, chain := range chains {
		if chain.Table != nil && chain.Table.Name == table.Name && chain.Name == linuxNFTOwnerMarkerChain {
			return true, nil
		}
	}
	return false, nil
}

func linuxNFTExpressions(rule linuxNFTRule) ([]expr.Any, error) {
	switch rule.kind {
	case linuxNFTRuleMasquerade:
		_, network, err := net.ParseCIDR(rule.sourceCIDR)
		if err != nil {
			return nil, fmt.Errorf("parse nftables source %s: %w", rule.sourceCIDR, err)
		}
		return []expr.Any{
			&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte(network.Mask), Xor: []byte{0, 0, 0, 0}},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: network.IP.To4()},
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: linuxNFTInterfaceName(rule.bridge)},
			&expr.Masq{},
		}, nil
	case linuxNFTRuleForwardIn:
		return linuxNFTForwardExpressions(expr.MetaKeyIIFNAME, rule.bridge), nil
	case linuxNFTRuleForwardOut:
		return linuxNFTForwardExpressions(expr.MetaKeyOIFNAME, rule.bridge), nil
	default:
		return nil, fmt.Errorf("unknown Linux nftables rule kind %d", rule.kind)
	}
}

func linuxNFTForwardExpressions(key expr.MetaKey, bridge string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: linuxNFTInterfaceName(bridge)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

func linuxNFTInterfaceName(name string) []byte {
	result := make([]byte, unix.IFNAMSIZ)
	copy(result, name)
	return result
}
