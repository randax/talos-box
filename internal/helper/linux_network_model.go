package helper

import (
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
)

const linuxBridgePrefix = "br-tbx"

type linuxLinkState struct {
	Name  string
	Addrs []net.Addr
}

type linuxSubnetInspector struct {
	Interfaces func() ([]linuxLinkState, error)
	Route      func(net.IP) (cluster.HostRoute, error)
}

type linuxNetworkOps interface {
	ListLinks() ([]linuxLinkState, error)
	Route(net.IP) (cluster.HostRoute, error)
	EnsureIPv4Forwarding() error
	EnsureInterfaceForwarding(name string) error
	EnsureManagedTaps() error
	EnsureBridge(name string) error
	EnsureBridgeSTP(name string, enabled bool) error
	EnsureBridgeAddress(name, cidr string) error
	EnsureLinkUp(name string) error
	CreateTap(name, bridge string) (*os.File, error)
}

type linuxNFTConverger interface {
	Converge(subnetIndexes []int) error
}

func bridgeNameForSubnet(index int) string {
	return fmt.Sprintf("%s%d", linuxBridgePrefix, index)
}

func tapNameForNode(subnetIndex int, clusterName, node string) string {
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(clusterName))
	_, _ = sum.Write([]byte{0})
	_, _ = sum.Write([]byte(node))
	return fmt.Sprintf("tbx%d-%08x", subnetIndex, sum.Sum32())
}

func subnetIndexFromTapName(name string) (int, bool) {
	if !strings.HasPrefix(name, "tbx") {
		return 0, false
	}
	indexText, hash, ok := strings.Cut(strings.TrimPrefix(name, "tbx"), "-")
	if !ok || len(hash) != 8 {
		return 0, false
	}
	index, err := strconv.Atoi(indexText)
	if err != nil || index < 0 || index > cluster.MaxSubnetIndex {
		return 0, false
	}
	if _, err := strconv.ParseUint(hash, 16, 32); err != nil {
		return 0, false
	}
	return index, true
}

func managedSubnetIndexes(links []linuxLinkState) []int {
	indexes := make([]int, 0, len(links))
	for _, link := range links {
		if index, ok := subnetIndexFromBridgeName(link.Name); ok {
			indexes = append(indexes, index)
			continue
		}
		if index, ok := subnetIndexFromTapName(link.Name); ok {
			indexes = append(indexes, index)
		}
	}
	slices.Sort(indexes)
	indexes = slices.Compact(indexes)
	return indexes
}

func subnetIndexFromBridgeName(name string) (int, bool) {
	if !strings.HasPrefix(name, linuxBridgePrefix) {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(name, linuxBridgePrefix))
	if err != nil || index < 0 || index > cluster.MaxSubnetIndex {
		return 0, false
	}
	return index, true
}

func linuxClusterSources(inspector linuxSubnetInspector) cluster.SubnetSources {
	return cluster.SubnetSources{
		Interfaces: func() ([]cluster.HostInterface, error) {
			links, err := inspector.Interfaces()
			if err != nil {
				return nil, err
			}
			result := make([]cluster.HostInterface, 0, len(links))
			for _, link := range links {
				name := link.Name
				if index, ok := subnetIndexFromBridgeName(link.Name); ok {
					name = fmt.Sprintf("bridge%d", 100+index)
				}
				result = append(result, cluster.HostInterface{Name: name, Addrs: link.Addrs})
			}
			return result, nil
		},
		Route: inspector.Route,
	}
}

func preflightLinuxSubnet(index int, inspector linuxSubnetInspector) (string, error) {
	sources := linuxClusterSources(inspector)
	warning, err := cluster.CheckSubnetIndex(index, sources)
	if err == nil && warning == "" {
		return "", nil
	}
	conflict := err
	if conflict == nil {
		conflict = fmt.Errorf("subnet %s is not safe to attach: %s", cluster.SubnetCIDR(index), warning)
	}
	hint, hintErr := lowestSafeLinuxSubnet(inspector)
	if hintErr != nil || hint == index {
		return "", conflict
	}
	return "", fmt.Errorf("%w; try subnet index %d (%s)", conflict, hint, cluster.SubnetCIDR(hint))
}

func lowestSafeLinuxSubnet(inspector linuxSubnetInspector) (int, error) {
	links, err := inspector.Interfaces()
	if err != nil {
		return 0, err
	}
	managed := make(map[int]struct{})
	for _, index := range managedSubnetIndexes(links) {
		managed[index] = struct{}{}
	}
	sources := linuxClusterSources(inspector)
	for index := 0; index <= cluster.MaxSubnetIndex; index++ {
		if _, used := managed[index]; used {
			continue
		}
		warning, err := cluster.CheckSubnetIndex(index, sources)
		if err == nil && warning == "" {
			return index, nil
		}
	}
	return 0, fmt.Errorf("no collision-free cluster subnet is available")
}

func convergeLinuxManagedState(netOps linuxNetworkOps, nft linuxNFTConverger) error {
	links, err := netOps.ListLinks()
	if err != nil {
		return fmt.Errorf("list helper-managed links: %w", err)
	}
	if err := netOps.EnsureIPv4Forwarding(); err != nil {
		return err
	}
	managed := managedSubnetIndexes(links)
	for _, index := range managed {
		if err := ensureLinuxBridge(netOps, index); err != nil {
			return err
		}
	}
	if err := netOps.EnsureManagedTaps(); err != nil {
		return err
	}
	return nft.Converge(managed)
}

func startLinuxAttachment(netOps linuxNetworkOps, nft linuxNFTConverger, subnetIndex int, clusterName, node string) (*os.File, error) {
	_, err := preflightLinuxSubnet(subnetIndex, linuxSubnetInspector{
		Interfaces: netOps.ListLinks,
		Route:      netOps.Route,
	})
	if err != nil {
		return nil, err
	}
	if err := netOps.EnsureIPv4Forwarding(); err != nil {
		return nil, err
	}
	if err := ensureLinuxBridge(netOps, subnetIndex); err != nil {
		return nil, err
	}
	links, err := netOps.ListLinks()
	if err != nil {
		return nil, fmt.Errorf("list helper-managed links: %w", err)
	}
	managed := managedSubnetIndexes(links)
	if !slices.Contains(managed, subnetIndex) {
		managed = append(managed, subnetIndex)
		slices.Sort(managed)
	}
	if err := nft.Converge(managed); err != nil {
		return nil, err
	}
	file, err := netOps.CreateTap(tapNameForNode(subnetIndex, clusterName, node), bridgeNameForSubnet(subnetIndex))
	if err != nil {
		return nil, err
	}
	return file, nil
}

func ensureLinuxBridge(netOps linuxNetworkOps, subnetIndex int) error {
	name := bridgeNameForSubnet(subnetIndex)
	if err := netOps.EnsureBridge(name); err != nil {
		return err
	}
	if err := netOps.EnsureBridgeSTP(name, false); err != nil {
		return err
	}
	if err := netOps.EnsureBridgeAddress(name, cluster.Gateway(subnetIndex)+"/24"); err != nil {
		return err
	}
	if err := netOps.EnsureLinkUp(name); err != nil {
		return err
	}
	if err := netOps.EnsureInterfaceForwarding(name); err != nil {
		return err
	}
	return nil
}
