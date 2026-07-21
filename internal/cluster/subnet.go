package cluster

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// HostInterface is the host-networking information relevant to subnet
// selection. It is intentionally small so callers can inject test data.
type HostInterface struct {
	Name  string
	Addrs []net.Addr
}

// HostRoute describes the route selected for one candidate guest address.
type HostRoute struct {
	Interface string
	Network   *net.IPNet
}

// SubnetSources supplies unprivileged host interface and route observations.
type SubnetSources struct {
	Interfaces func() ([]HostInterface, error)
	Route      func(net.IP) (HostRoute, error)
}

type subnetInspection struct {
	conflict string
	warning  string
}

// SystemSubnetSources returns the real host-network readers used by tbxd.
func SystemSubnetSources() SubnetSources {
	return SubnetSources{
		Interfaces: systemInterfaces,
		Route:      systemRoute,
	}
}

// LowestUsableSubnetIndex returns the first unallocated subnet that does not
// collide with a host interface or a specific non-vmnet route. A broad route
// is usable but reported as a warning because a VPN may capture guest traffic.
func LowestUsableSubnetIndex(clusters []Cluster, sources SubnetSources) (int, string, error) {
	used := allocatedSubnetIndexes(clusters)
	hadUnallocated := false
	for _, allocated := range used {
		if !allocated {
			hadUnallocated = true
			break
		}
	}
	if !hadUnallocated {
		return 0, "", errors.New("all cluster subnets are allocated")
	}
	interfaces, err := readHostInterfaces(sources)
	if err != nil {
		return 0, "", err
	}

	for index, allocated := range used {
		if allocated {
			continue
		}
		inspection, err := inspectSubnet(index, interfaces, sources.Route, false)
		if err != nil {
			return 0, "", err
		}
		if inspection.conflict == "" {
			return index, inspection.warning, nil
		}
	}
	return 0, "", errors.New("all cluster subnets overlap existing host interfaces or routes")
}

// CheckSubnetIndex verifies that an existing cluster's subnet can be attached.
func CheckSubnetIndex(index int, sources SubnetSources) (string, error) {
	if index < 0 || index > MaxSubnetIndex {
		return "", fmt.Errorf("subnet index must be between 0 and %d", MaxSubnetIndex)
	}
	interfaces, err := readHostInterfaces(sources)
	if err != nil {
		return "", err
	}
	inspection, err := inspectSubnet(index, interfaces, sources.Route, true)
	if err != nil {
		return "", err
	}
	if inspection.conflict != "" {
		return "", fmt.Errorf("subnet %s conflicts with %s", SubnetCIDR(index), inspection.conflict)
	}
	return inspection.warning, nil
}

func allocatedSubnetIndexes(clusters []Cluster) []bool {
	used := make([]bool, MaxSubnetIndex+1)
	for _, item := range clusters {
		if item.SubnetIndex >= 0 && item.SubnetIndex <= MaxSubnetIndex {
			used[item.SubnetIndex] = true
		}
	}
	return used
}

func readHostInterfaces(sources SubnetSources) ([]HostInterface, error) {
	if sources.Interfaces == nil {
		return nil, errors.New("host interface source is not configured")
	}
	interfaces, err := sources.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("inspect host interfaces: %w", err)
	}
	return interfaces, nil
}

func inspectSubnet(
	index int,
	interfaces []HostInterface,
	routeSource func(net.IP) (HostRoute, error),
	allowTalosBoxBridge bool,
) (subnetInspection, error) {
	_, candidate, err := net.ParseCIDR(SubnetCIDR(index))
	if err != nil {
		return subnetInspection{}, err
	}
	for _, current := range interfaces {
		for _, address := range current.Addrs {
			ip, network, ok := ipv4Network(address)
			if !ok || !networksOverlap(candidate, network) {
				continue
			}
			if allowTalosBoxBridge && isTalosBoxBridge(current.Name, ip, network, index) {
				continue
			}
			return subnetInspection{conflict: fmt.Sprintf("interface %s address %s", current.Name, address)}, nil
		}
	}

	if routeSource == nil {
		return subnetInspection{}, errors.New("host route source is not configured")
	}
	destination := net.ParseIP(fmt.Sprintf("172.30.%d.2", index)).To4()
	route, err := routeSource(destination)
	if err != nil {
		return subnetInspection{}, fmt.Errorf("inspect route to %s: %w", destination, err)
	}
	if route.Interface == "" && route.Network == nil {
		return subnetInspection{}, nil
	}
	if route.Interface == "" || route.Network == nil {
		return subnetInspection{}, fmt.Errorf("inspect route to %s: incomplete route information", destination)
	}
	if strings.HasPrefix(route.Interface, "bridge") || !networksOverlap(candidate, route.Network) {
		return subnetInspection{}, nil
	}
	ones, bits := route.Network.Mask.Size()
	if bits != 32 || ones < 0 {
		return subnetInspection{}, fmt.Errorf("inspect route to %s: invalid IPv4 route mask", destination)
	}
	if ones == 0 {
		if strings.HasPrefix(route.Interface, "utun") {
			return subnetInspection{warning: subnetRouteWarning(index, route.Interface)}, nil
		}
		return subnetInspection{}, nil
	}
	if ones >= 24 {
		return subnetInspection{conflict: fmt.Sprintf("route %s through interface %s", route.Network, route.Interface)}, nil
	}
	return subnetInspection{warning: subnetRouteWarning(index, route.Interface)}, nil
}

func subnetRouteWarning(index int, interfaceName string) string {
	return fmt.Sprintf(
		"subnet %s is covered by a broader route through %s; VPN or corporate routing may capture cluster traffic",
		SubnetCIDR(index), interfaceName,
	)
}

func ipv4Network(address net.Addr) (net.IP, *net.IPNet, bool) {
	switch value := address.(type) {
	case *net.IPNet:
		ip := value.IP.To4()
		if ip == nil {
			return nil, nil, false
		}
		return ip, &net.IPNet{IP: ip.Mask(value.Mask), Mask: value.Mask}, true
	case *net.IPAddr:
		ip := value.IP.To4()
		if ip == nil {
			return nil, nil, false
		}
		return ip, &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}, true
	}
	ip, network, err := net.ParseCIDR(address.String())
	if err != nil || ip.To4() == nil {
		return nil, nil, false
	}
	return ip.To4(), network, true
}

func networksOverlap(left, right *net.IPNet) bool {
	return left.Contains(right.IP) || right.Contains(left.IP)
}

func isTalosBoxBridge(name string, ip net.IP, network *net.IPNet, index int) bool {
	ones, bits := network.Mask.Size()
	bridgeIndex, err := strconv.Atoi(strings.TrimPrefix(name, "bridge"))
	return err == nil && bridgeIndex >= 100 && bits == 32 && ones == 24 && ip.Equal(net.ParseIP(Gateway(index)))
}

func systemInterfaces() ([]HostInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make([]HostInterface, 0, len(interfaces))
	for _, current := range interfaces {
		addresses, err := current.Addrs()
		if err != nil {
			return nil, fmt.Errorf("read addresses for %s: %w", current.Name, err)
		}
		result = append(result, HostInterface{Name: current.Name, Addrs: addresses})
	}
	return result, nil
}

func systemRoute(destination net.IP) (HostRoute, error) {
	output, err := exec.Command("/sbin/route", "-n", "get", destination.String()).CombinedOutput()
	if err != nil {
		if routeNotFound(output) {
			return HostRoute{}, nil
		}
		return HostRoute{}, fmt.Errorf("run /sbin/route: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return parseHostRoute(output, destination)
}

func routeNotFound(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "not in table") || strings.Contains(message, "route has not been found")
}

func parseHostRoute(output []byte, queried net.IP) (HostRoute, error) {
	fields := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if fields["interface"] == "" {
		return HostRoute{}, errors.New("route output did not include an interface")
	}

	destination := fields["destination"]
	if destination == "default" {
		return HostRoute{Interface: fields["interface"], Network: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}}, nil
	}
	ip := net.ParseIP(destination).To4()
	if ip == nil {
		return HostRoute{}, fmt.Errorf("route output included invalid destination %q", destination)
	}
	mask, err := parseRouteMask(fields["mask"])
	if err != nil {
		return HostRoute{}, err
	}
	if mask == nil {
		if !ip.Equal(queried) {
			return HostRoute{}, errors.New("route output omitted the network mask")
		}
		mask = net.CIDRMask(32, 32)
	}
	return HostRoute{Interface: fields["interface"], Network: &net.IPNet{IP: ip.Mask(mask), Mask: mask}}, nil
}

func parseRouteMask(value string) (net.IPMask, error) {
	if value == "" {
		return nil, nil
	}
	if parsed := net.ParseIP(value).To4(); parsed != nil {
		return net.IPMask(parsed), nil
	}
	if strings.HasPrefix(value, "0x") {
		parsed, err := strconv.ParseUint(strings.TrimPrefix(value, "0x"), 16, 32)
		if err == nil {
			mask := make(net.IPMask, net.IPv4len)
			binary.BigEndian.PutUint32(mask, uint32(parsed))
			return mask, nil
		}
	}
	return nil, fmt.Errorf("route output included invalid mask %q", value)
}
