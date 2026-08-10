//go:build linux

package cluster

// LookupIP returns the authoritative persisted reservation for mac on Linux.
func LookupIP(mac string, subnetIndex int) string {
	clusters, err := List()
	if err != nil {
		return ""
	}
	return lookupReservedIP(clusters, mac, subnetIndex)
}
