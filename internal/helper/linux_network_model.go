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

const linuxLinkAliasPrefix = "talos-box:"

type linuxLinkState struct {
	Name  string
	Alias string
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
	EnsureManagedTaps(subnetIndexes []int) error
	EnsureBridge(name string) error
	DeleteBridge(name string) (bool, error)
	EnsureBridgeSTP(name string, enabled bool) error
	EnsureBridgeAddress(name, cidr string) error
	EnsureLinkUp(name string) error
	CreateTap(name, bridge string) (*os.File, error)
}

type linuxNFTConverger interface {
	Converge(subnetIndexes []int) error
}

func bridgeNameForSubnet(index int) string {
	return cluster.LinuxBridgeName(index)
}

func bridgeAliasForSubnet(index int) string {
	return cluster.LinuxBridgeAlias(index)
}

func tapAliasForSubnet(index int) string {
	return fmt.Sprintf("%stap:%d", linuxLinkAliasPrefix, index)
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
		if index, ok := subnetIndexFromBridgeName(link.Name); ok && link.Alias == bridgeAliasForSubnet(index) {
			indexes = append(indexes, index)
			continue
		}
		if index, ok := subnetIndexFromTapName(link.Name); ok && link.Alias == tapAliasForSubnet(index) {
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
	ownedBridges := make(map[string]int)
	return cluster.SubnetSources{
		Interfaces: func() ([]cluster.HostInterface, error) {
			links, err := inspector.Interfaces()
			if err != nil {
				return nil, err
			}
			result := make([]cluster.HostInterface, 0, len(links))
			for _, link := range links {
				name := link.Name
				if index, ok := subnetIndexFromBridgeName(link.Name); ok && link.Alias == bridgeAliasForSubnet(index) {
					ownedBridges[link.Name] = index
					name = fmt.Sprintf("bridge%d", 100+index)
				}
				result = append(result, cluster.HostInterface{Name: name, Addrs: link.Addrs})
			}
			return result, nil
		},
		Route: func(destination net.IP) (cluster.HostRoute, error) {
			route, err := inspector.Route(destination)
			if err != nil {
				return cluster.HostRoute{}, err
			}
			if index, ok := ownedBridges[route.Interface]; ok {
				route.Interface = fmt.Sprintf("bridge%d", 100+index)
			} else if _, looksManaged := subnetIndexFromBridgeName(route.Interface); looksManaged {
				route.Interface = "foreign:" + route.Interface
			}
			return route, nil
		},
	}
}

func preflightLinuxSubnet(index int, inspector linuxSubnetInspector, reserved []int) (string, error) {
	sources := linuxClusterSources(inspector)
	warning, err := cluster.CheckSubnetIndex(index, sources)
	if err == nil && warning == "" {
		return "", nil
	}
	conflict := err
	if conflict == nil {
		conflict = fmt.Errorf("subnet %s is not safe to attach: %s", cluster.SubnetCIDR(index), warning)
	}
	hint, hintErr := lowestSafeLinuxSubnet(inspector, reserved)
	if hintErr != nil || hint == index {
		return "", conflict
	}
	return "", fmt.Errorf("%w; try subnet index %d (%s)", conflict, hint, cluster.SubnetCIDR(hint))
}

func lowestSafeLinuxSubnet(inspector linuxSubnetInspector, reserved []int) (int, error) {
	links, err := inspector.Interfaces()
	if err != nil {
		return 0, err
	}
	managed := make(map[int]struct{})
	for _, index := range normalizeLinuxSubnetIndexes(reserved) {
		managed[index] = struct{}{}
	}
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

func convergeLinuxManagedState(netOps linuxNetworkOps, nft linuxNFTConverger, configured []int) error {
	desired := normalizeLinuxSubnetIndexes(configured)
	inspector := linuxSubnetInspector{Interfaces: netOps.ListLinks, Route: netOps.Route}
	for _, index := range desired {
		if _, err := preflightLinuxSubnet(index, inspector, desired); err != nil {
			return err
		}
	}
	if err := netOps.EnsureIPv4Forwarding(); err != nil {
		return err
	}
	for _, index := range desired {
		if err := ensureLinuxBridge(netOps, index); err != nil {
			return err
		}
	}
	if err := netOps.EnsureManagedTaps(desired); err != nil {
		return err
	}
	return nft.Converge(desired)
}

func startLinuxAttachment(netOps linuxNetworkOps, nft linuxNFTConverger, configured []int, subnetIndex int, clusterName, node string) (*os.File, error) {
	_, err := preflightLinuxSubnet(subnetIndex, linuxSubnetInspector{
		Interfaces: netOps.ListLinks,
		Route:      netOps.Route,
	}, configured)
	if err != nil {
		return nil, err
	}
	if err := netOps.EnsureIPv4Forwarding(); err != nil {
		return nil, err
	}
	if err := ensureLinuxBridge(netOps, subnetIndex); err != nil {
		return nil, err
	}
	desired := normalizeLinuxSubnetIndexes(configured)
	if !slices.Contains(desired, subnetIndex) {
		desired = append(desired, subnetIndex)
		slices.Sort(desired)
	}
	if err := nft.Converge(desired); err != nil {
		return nil, err
	}
	file, err := netOps.CreateTap(tapNameForNode(subnetIndex, clusterName, node), bridgeNameForSubnet(subnetIndex))
	if err != nil {
		return nil, err
	}
	return file, nil
}

// teardownLinuxBridge removes the bridge that carries a subnet's gateway
// address, once the last cluster on that subnet is gone. It reports whether a
// bridge was there to remove: an absent bridge is a success, so a repeated
// destroy — or a destroy on a host that never built the bridge — is a no-op
// rather than an error.
func teardownLinuxBridge(netOps linuxNetworkOps, subnetIndex int) (bool, error) {
	if subnetIndex < 0 || subnetIndex > cluster.MaxSubnetIndex {
		return false, fmt.Errorf("subnet index %d is outside 0..%d", subnetIndex, cluster.MaxSubnetIndex)
	}
	return netOps.DeleteBridge(bridgeNameForSubnet(subnetIndex))
}

func normalizeLinuxSubnetIndexes(indexes []int) []int {
	normalized := make([]int, 0, len(indexes))
	for _, index := range indexes {
		if index >= 0 && index <= cluster.MaxSubnetIndex {
			normalized = append(normalized, index)
		}
	}
	slices.Sort(normalized)
	return slices.Compact(normalized)
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
