//go:build darwin

package cluster

import "os"

const dhcpLeasesPath = "/var/db/dhcpd_leases"

func darwinLeaseIP(mac string, subnetIndex int) string {
	data, err := os.ReadFile(dhcpLeasesPath)
	if err != nil {
		return ""
	}
	return parseLeaseIP(string(data), mac, subnetIndex)
}
