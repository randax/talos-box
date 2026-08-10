package cluster

import (
	"fmt"
	"net"
	"strings"
)

// Reservation is the authoritative network identity assigned to one node.
type Reservation struct {
	MAC         net.HardwareAddr
	IP          net.IP
	SubnetIndex int
}

// ReservationTable indexes persisted node reservations by canonical MAC.
type ReservationTable struct {
	byMAC map[string]Reservation
}

// NewReservationTable validates and indexes all node reservations.
func NewReservationTable(clusters []Cluster) (ReservationTable, error) {
	byMAC := make(map[string]Reservation)
	byIP := make(map[string]string)
	for _, item := range clusters {
		if item.SubnetIndex < 0 || item.SubnetIndex > MaxSubnetIndex {
			return ReservationTable{}, fmt.Errorf("cluster %q has invalid subnet index %d", item.Name, item.SubnetIndex)
		}
		for _, node := range item.Nodes {
			mac, err := canonicalMAC(node.MAC)
			if err != nil {
				return ReservationTable{}, fmt.Errorf("node %s/%s: %w", item.Name, node.Name, err)
			}
			ip := net.ParseIP(node.IP).To4()
			if ip == nil || !validReservationIP(ip, item.SubnetIndex) {
				return ReservationTable{}, fmt.Errorf("node %s/%s has invalid reservation %q for subnet %s", item.Name, node.Name, node.IP, SubnetCIDR(item.SubnetIndex))
			}
			key := mac.String()
			if previous, ok := byMAC[key]; ok {
				return ReservationTable{}, fmt.Errorf("MAC %s is reserved by both %s and %s/%s", key, previous.IP, item.Name, node.Name)
			}
			ipKey := ip.String()
			if previous, ok := byIP[ipKey]; ok {
				return ReservationTable{}, fmt.Errorf("IP %s is reserved by both %s and %s/%s", ipKey, previous, item.Name, node.Name)
			}
			byMAC[key] = Reservation{MAC: mac, IP: append(net.IP(nil), ip...), SubnetIndex: item.SubnetIndex}
			byIP[ipKey] = item.Name + "/" + node.Name
		}
	}
	return ReservationTable{byMAC: byMAC}, nil
}

// Lookup returns the reservation for mac.
func (t ReservationTable) Lookup(mac string) (Reservation, bool) {
	parsed, err := canonicalMAC(mac)
	if err != nil {
		return Reservation{}, false
	}
	reservation, ok := t.byMAC[parsed.String()]
	if !ok {
		return Reservation{}, false
	}
	reservation.MAC = append(net.HardwareAddr(nil), reservation.MAC...)
	reservation.IP = append(net.IP(nil), reservation.IP...)
	return reservation, true
}

// LookupIP returns a copy of the reserved IPv4 address for mac.
func (t ReservationTable) LookupIP(mac string) net.IP {
	reservation, ok := t.Lookup(mac)
	if !ok {
		return nil
	}
	return reservation.IP
}

func lookupReservedIP(clusters []Cluster, mac string, subnetIndex int) string {
	table, err := NewReservationTable(clusters)
	if err != nil {
		return ""
	}
	reservation, ok := table.Lookup(mac)
	if !ok || reservation.SubnetIndex != subnetIndex {
		return ""
	}
	return reservation.IP.String()
}

func ensureNodeReservations(item *Cluster) (bool, error) {
	used := make(map[string]struct{}, len(item.Nodes))
	for _, node := range item.Nodes {
		if node.IP == "" {
			continue
		}
		ip := net.ParseIP(node.IP).To4()
		if ip == nil || !validReservationIP(ip, item.SubnetIndex) {
			return false, fmt.Errorf("node %q has invalid reservation %q for subnet %s", node.Name, node.IP, SubnetCIDR(item.SubnetIndex))
		}
		key := ip.String()
		if _, exists := used[key]; exists {
			return false, fmt.Errorf("reservation %s is assigned more than once", key)
		}
		used[key] = struct{}{}
	}
	migrated := false
	for index := range item.Nodes {
		if item.Nodes[index].IP != "" {
			continue
		}
		ip, err := lowestFreeReservation(item.SubnetIndex, used)
		if err != nil {
			return false, err
		}
		item.Nodes[index].IP = ip
		used[ip] = struct{}{}
		migrated = true
	}
	return migrated, nil
}

func nextReservationIP(item Cluster) (string, error) {
	if _, err := ensureNodeReservations(&item); err != nil {
		return "", err
	}
	used := make(map[string]struct{}, len(item.Nodes))
	for _, node := range item.Nodes {
		used[node.IP] = struct{}{}
	}
	return lowestFreeReservation(item.SubnetIndex, used)
}

func lowestFreeReservation(subnetIndex int, used map[string]struct{}) (string, error) {
	for host := firstNodeHost; host <= lastNodeHost; host++ {
		candidate := reservationIP(subnetIndex, host)
		if _, exists := used[candidate]; !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("subnet %s has no free node reservations", SubnetCIDR(subnetIndex))
}

func reservationIP(subnetIndex, host int) string {
	return fmt.Sprintf("172.30.%d.%d", subnetIndex, host)
}

func validReservationIP(ip net.IP, subnetIndex int) bool {
	return subnetIndex >= 0 && subnetIndex <= MaxSubnetIndex && len(ip) == net.IPv4len &&
		ip[0] == 172 && ip[1] == 30 && int(ip[2]) == subnetIndex &&
		ip[3] >= firstNodeHost && ip[3] <= lastNodeHost
}

func canonicalMAC(value string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("invalid MAC %q: %w", value, err)
	}
	if len(mac) != 6 {
		return nil, fmt.Errorf("invalid MAC %q: must contain 6 octets", value)
	}
	return mac, nil
}
