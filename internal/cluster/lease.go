package cluster

import (
	"bufio"
	"fmt"
	"net"
	"strings"
)

// LeaseIP is kept as a compatibility alias for callers outside this module.
// New code should use LookupIP, whose implementation is platform-specific.
func LeaseIP(mac string, subnetIndex int) string {
	return LookupIP(mac, subnetIndex)
}

func parseLeaseIP(data, mac string, subnetIndex int) string {
	want, err := leaseMAC(mac)
	if err != nil {
		return ""
	}
	var ip string
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "{":
			ip = ""
		case strings.HasPrefix(line, "ip_address="):
			ip = strings.TrimPrefix(line, "ip_address=")
		case strings.HasPrefix(line, "hw_address=1,"):
			got := strings.ToLower(strings.TrimPrefix(line, "hw_address=1,"))
			if ip != "" && got == want && validLeaseIP(ip, subnetIndex) {
				return ip
			}
		}
	}
	return ""
}

func validLeaseIP(value string, subnetIndex int) bool {
	ip := net.ParseIP(value).To4()
	return subnetIndex >= 0 && subnetIndex <= 255 && ip != nil &&
		ip[0] == 172 && ip[1] == 30 && int(ip[2]) == subnetIndex && ip[3] >= 2 && ip[3] <= 179
}

// vmnet's lease file formats every octet with %x rather than %02x.
func leaseMAC(mac string) (string, error) {
	hardwareAddr, err := net.ParseMAC(mac)
	if err != nil {
		return "", err
	}
	if len(hardwareAddr) != 6 {
		return "", fmt.Errorf("MAC address must contain 6 octets")
	}
	parts := make([]string, len(hardwareAddr))
	for i, octet := range hardwareAddr {
		parts[i] = fmt.Sprintf("%x", octet)
	}
	return strings.Join(parts, ":"), nil
}
