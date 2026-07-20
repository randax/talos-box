package vm

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
)

const dhcpLeasesPath = "/var/db/dhcpd_leases"

// LeaseIP returns the current vmnet DHCP lease for mac, or an empty string if
// the lease file cannot be read or does not contain the address.
func LeaseIP(mac string) string {
	data, err := os.ReadFile(dhcpLeasesPath)
	if err != nil {
		return ""
	}

	return parseLeaseIP(string(data), mac)
}

func parseLeaseIP(data, mac string) string {
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
			if ip != "" && got == want {
				return ip
			}
		}
	}

	return ""
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
