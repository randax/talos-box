//go:build linux

package helper

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"sync"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/randax/talos-box/internal/cluster"
)

type dhcpListener interface {
	Serve() error
	Close() error
}

// dhcpEntry is a running per-subnet DHCP server plus the bridge it is bound
// to. server4.NewServer binds with SO_BINDTODEVICE, which the kernel resolves
// to an ifindex once, at setsockopt time: a bridge that is deleted and
// recreated under the same name gets a new ifindex, and the old socket keeps
// listening on an interface that no longer exists. Recording the ifindex lets
// Converge notice that and rebind.
type dhcpEntry struct {
	listener dhcpListener
	ifindex  int
}

type linuxDHCPManager struct {
	mu      sync.Mutex
	servers map[int]dhcpEntry
	load    func() []cluster.Cluster
	// extra names subnets that have no synced cluster yet — live attachments.
	extra   func() []int
	listen  func(int, server4.Handler) (dhcpListener, error)
	ifindex func(string) (int, error)
}

func newPlatformDHCPManager(load func() []cluster.Cluster, extra func() []int) dhcpManager {
	return &linuxDHCPManager{
		servers: make(map[int]dhcpEntry),
		load:    load,
		extra:   extra,
		listen:  listenDHCP,
		ifindex: bridgeIfindex,
	}
}

func (m *linuxDHCPManager) Converge() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	clusters := m.load()
	if _, err := cluster.NewReservationTable(clusters); err != nil {
		return fmt.Errorf("validate DHCP reservations: %w", err)
	}
	desired := make([]int, 0, len(clusters))
	for _, item := range clusters {
		desired = append(desired, item.SubnetIndex)
	}
	if m.extra != nil {
		desired = append(desired, m.extra()...)
	}
	desired = normalizeLinuxSubnetIndexes(desired)

	started := make(map[int]dhcpEntry)
	for _, subnetIndex := range desired {
		// The lookup precedes the bind so a bridge recreated between the two
		// records the older ifindex: the next Converge rebinds once, where the
		// reverse order would record the new ifindex for a socket bound to the
		// old one and never notice.
		ifindex, err := m.ifindex(bridgeNameForSubnet(subnetIndex))
		if err != nil {
			// An absent bridge fails the bind below with the canonical error.
			// Until then ifindex 0 — never a real interface — keeps the entry
			// marked stale so a later Converge rebinds it.
			ifindex = 0
		}
		if entry, exists := m.servers[subnetIndex]; exists {
			if entry.ifindex == ifindex {
				continue
			}
			log.Printf("rebinding DHCP on %s: bridge was recreated", bridgeNameForSubnet(subnetIndex))
			if err := closeDHCPListener(subnetIndex, entry.listener); err != nil {
				closeDHCPEntries(started)
				return err
			}
			delete(m.servers, subnetIndex)
		}
		listener, err := m.listen(subnetIndex, m.handler(subnetIndex))
		if err != nil {
			closeDHCPEntries(started)
			return fmt.Errorf("listen for DHCP on %s: %w", bridgeNameForSubnet(subnetIndex), err)
		}
		started[subnetIndex] = dhcpEntry{listener: listener, ifindex: ifindex}
		go serveDHCP(subnetIndex, listener)
	}
	for subnetIndex, entry := range m.servers {
		if slices.Contains(desired, subnetIndex) {
			continue
		}
		if err := closeDHCPListener(subnetIndex, entry.listener); err != nil {
			return err
		}
		delete(m.servers, subnetIndex)
	}
	for subnetIndex, entry := range started {
		m.servers[subnetIndex] = entry
	}
	return nil
}

// Release stops the DHCP server a subnet's bridge carried. The socket is bound
// to that bridge's ifindex, so it must go with the bridge: a subnet index that
// is later reused builds a new bridge, and Converge binds a fresh socket to it.
// A subnet without a listener is success.
func (m *linuxDHCPManager) Release(subnetIndex int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, exists := m.servers[subnetIndex]
	if !exists {
		return nil
	}
	delete(m.servers, subnetIndex)
	return closeDHCPListener(subnetIndex, entry.listener)
}

func (m *linuxDHCPManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result error
	for subnetIndex, entry := range m.servers {
		result = errors.Join(result, closeDHCPListener(subnetIndex, entry.listener))
		delete(m.servers, subnetIndex)
	}
	return result
}

// closeDHCPListener stops one subnet's listener. A socket that is already
// closed is success, the same rule an absent bridge follows on teardown.
func closeDHCPListener(subnetIndex int, listener dhcpListener) error {
	if listener == nil {
		return nil
	}
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("stop DHCP on %s: %w", bridgeNameForSubnet(subnetIndex), err)
	}
	return nil
}

func closeDHCPEntries(entries map[int]dhcpEntry) {
	for _, entry := range entries {
		_ = entry.listener.Close()
	}
}

func bridgeIfindex(name string) (int, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return 0, fmt.Errorf("look up interface %s: %w", name, err)
	}
	return iface.Index, nil
}

func (m *linuxDHCPManager) handler(subnetIndex int) server4.Handler {
	return func(connection net.PacketConn, peer net.Addr, request *dhcpv4.DHCPv4) {
		reservations, err := cluster.NewReservationTable(m.load())
		if err != nil {
			log.Printf("validate DHCP reservations for %s: %v", bridgeNameForSubnet(subnetIndex), err)
			return
		}
		reply, err := buildDHCPReply(request, reservations, subnetIndex)
		if err != nil {
			log.Printf("build DHCP reply on %s: %v", bridgeNameForSubnet(subnetIndex), err)
			return
		}
		if reply == nil {
			return
		}
		if _, err := connection.WriteTo(reply.ToBytes(), peer); err != nil {
			log.Printf("send DHCP reply on %s: %v", bridgeNameForSubnet(subnetIndex), err)
		}
	}
}

func listenDHCP(subnetIndex int, handler server4.Handler) (dhcpListener, error) {
	return server4.NewServer(
		bridgeNameForSubnet(subnetIndex),
		&net.UDPAddr{IP: net.IPv4zero, Port: dhcpv4.ServerPort},
		handler,
	)
}

func serveDHCP(subnetIndex int, listener dhcpListener) {
	if err := listener.Serve(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
		log.Printf("DHCP server on %s stopped: %v", bridgeNameForSubnet(subnetIndex), err)
	}
}
