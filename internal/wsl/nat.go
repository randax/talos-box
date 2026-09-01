package wsl

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const procNetRoute = "/proc/net/route"

type interfaceAddrs func(string) ([]net.Addr, error)

func systemInterfaceAddrs(name string) ([]net.Addr, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	return iface.Addrs()
}

func natPrefixFrom(readFile ReadFile, addrs interfaceAddrs) (string, error) {
	if readFile == nil || addrs == nil {
		return "", errors.New("NAT route reader unavailable")
	}
	content, err := readFile(procNetRoute)
	if err != nil {
		return "", fmt.Errorf("read default routes: %w", err)
	}
	iface, err := lowestMetricDefaultRoute(content)
	if err != nil {
		return "", err
	}
	addresses, err := addrs(iface)
	if err != nil {
		return "", fmt.Errorf("inspect interface %s: %w", iface, err)
	}
	for _, address := range addresses {
		ip, network, err := net.ParseCIDR(address.String())
		if err != nil || ip.IsLoopback() || ip.To4() == nil {
			continue
		}
		ones, bits := network.Mask.Size()
		if bits != 32 || ones >= 32 {
			continue
		}
		network.IP = ip.Mask(network.Mask)
		return network.String(), nil
	}
	return "", fmt.Errorf("interface %s has no usable IPv4 prefix", iface)
}

func lowestMetricDefaultRoute(content []byte) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	if !scanner.Scan() {
		return "", errors.New("route table is empty")
	}
	bestInterface := ""
	bestMetric := uint64(^uint(0))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			return "", fmt.Errorf("malformed route row %q", line)
		}
		destination, err := linuxRouteIPv4(fields[1])
		if err != nil {
			return "", fmt.Errorf("parse route destination %q: %w", fields[1], err)
		}
		if _, err := linuxRouteIPv4(fields[2]); err != nil {
			return "", fmt.Errorf("parse route gateway %q: %w", fields[2], err)
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil {
			return "", fmt.Errorf("parse route flags %q: %w", fields[3], err)
		}
		metric, err := strconv.ParseUint(fields[6], 10, 32)
		if err != nil {
			return "", fmt.Errorf("parse route metric %q: %w", fields[6], err)
		}
		mask, err := linuxRouteIPv4(fields[7])
		if err != nil {
			return "", fmt.Errorf("parse route mask %q: %w", fields[7], err)
		}
		if !destination.Equal(net.IPv4zero) || !mask.Equal(net.IPv4zero) || flags&0x1 == 0 {
			continue
		}
		if bestInterface == "" || metric < bestMetric {
			bestInterface, bestMetric = fields[0], metric
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan route table: %w", err)
	}
	if bestInterface == "" {
		return "", errors.New("no UP IPv4 default route")
	}
	return bestInterface, nil
}

func linuxRouteIPv4(value string) (net.IP, error) {
	bytes, err := hex.DecodeString(value)
	if err != nil || len(bytes) != net.IPv4len {
		return nil, fmt.Errorf("invalid little-endian IPv4 hexadecimal value")
	}
	return net.IPv4(bytes[3], bytes[2], bytes[1], bytes[0]), nil
}
