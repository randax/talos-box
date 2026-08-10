//go:build darwin

package cluster

// LookupIP returns vmnet's current DHCP lease on Darwin.
func LookupIP(mac string, subnetIndex int) string {
	return darwinLeaseIP(mac, subnetIndex)
}
