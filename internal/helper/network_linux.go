//go:build linux

package helper

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
func StartInterface(subnetIndex int, clusterName, node string) (*platformAttachment, error) {
	file, err := startLinuxAttachment(linuxNetwork, linuxNFT, subnetIndex, clusterName, node)
	if err != nil {
		return nil, err
	}
	return &platformAttachment{
		Kind: AttachmentTapFD,
		FD:   int(file.Fd()),
		stop: file.Close,
	}, nil
}

func convergeNetworking() error {
	return convergeLinuxManagedState(linuxNetwork, linuxNFT)
}

func enableForwarding() error {
	return linuxNetwork.EnsureIPv4Forwarding()
}

type realLinuxNetworkOps struct{}

func (realLinuxNetworkOps) ListLinks() ([]linuxLinkState, error) {
	interfaces, err := cluster.SystemSubnetSources().Interfaces()
	if err != nil {
		return nil, err
	}
	result := make([]linuxLinkState, 0, len(interfaces))
	for _, current := range interfaces {
		result = append(result, linuxLinkState{Name: current.Name, Addrs: current.Addrs})
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

func (realLinuxNetworkOps) EnsureManagedTaps() error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("dump links while converging taps: %w", err)
	}
	for _, link := range links {
		index, ok := subnetIndexFromTapName(link.Attrs().Name)
		if !ok {
			continue
		}
		tap, ok := link.(*netlink.Tuntap)
		if !ok || tap.Mode != netlink.TUNTAP_MODE_TAP {
			return fmt.Errorf("helper-owned interface name %s has type %s, want tap", link.Attrs().Name, link.Type())
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

func ensureLinuxSysctl(path string) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if strings.TrimSpace(string(current)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1\n"), 0); err != nil {
		return fmt.Errorf("enable IPv4 forwarding in %s: %w", path, err)
	}
	return nil
}

func (realLinuxNetworkOps) EnsureBridge(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("inspect bridge %s: %w", name, err)
		}
		if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
			return fmt.Errorf("create bridge %s: %w", name, err)
		}
		link, err = netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("inspect created bridge %s: %w", name, err)
		}
	}
	if _, ok := link.(*netlink.Bridge); !ok {
		return fmt.Errorf("interface %s already exists with type %s, want bridge", name, link.Type())
	}
	return nil
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

func (realLinuxNetworkOps) CreateTap(name, bridgeName string) (*os.File, error) {
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

	tap := &netlink.Tuntap{
		LinkAttrs:  netlink.LinkAttrs{Name: name, MasterIndex: bridge.Attrs().Index},
		Mode:       netlink.TUNTAP_MODE_TAP,
		Flags:      netlink.TUNTAP_DEFAULTS,
		NonPersist: true,
		Queues:     1,
		Owner:      uint32(os.Geteuid()),
		Group:      uint32(os.Getegid()),
	}
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
			connection.DelTable(table)
		}
	}

	table := connection.AddTable(&nftables.Table{Name: plan.tableName, Family: nftables.TableFamilyINet})
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

func linuxNFTExpressions(rule linuxNFTRule) ([]expr.Any, error) {
	switch rule.kind {
	case linuxNFTRuleMasquerade:
		_, network, err := net.ParseCIDR(rule.sourceCIDR)
		if err != nil {
			return nil, fmt.Errorf("parse nftables source %s: %w", rule.sourceCIDR, err)
		}
		return []expr.Any{
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
