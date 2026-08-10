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

type linuxDHCPManager struct {
	mu      sync.Mutex
	servers map[int]dhcpListener
	load    func() ([]cluster.Cluster, error)
	listen  func(int, server4.Handler) (dhcpListener, error)
}

func newPlatformDHCPManager() dhcpManager {
	return &linuxDHCPManager{
		servers: make(map[int]dhcpListener),
		load:    cluster.List,
		listen:  listenDHCP,
	}
}

func (m *linuxDHCPManager) Converge() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	clusters, err := m.load()
	if err != nil {
		return fmt.Errorf("load DHCP reservations: %w", err)
	}
	if _, err := cluster.NewReservationTable(clusters); err != nil {
		return fmt.Errorf("validate DHCP reservations: %w", err)
	}
	desired := make([]int, 0, len(clusters))
	for _, item := range clusters {
		desired = append(desired, item.SubnetIndex)
	}
	desired = normalizeLinuxSubnetIndexes(desired)

	started := make(map[int]dhcpListener)
	for _, subnetIndex := range desired {
		if _, exists := m.servers[subnetIndex]; exists {
			continue
		}
		listener, err := m.listen(subnetIndex, m.handler(subnetIndex))
		if err != nil {
			for _, current := range started {
				_ = current.Close()
			}
			return fmt.Errorf("listen for DHCP on %s: %w", bridgeNameForSubnet(subnetIndex), err)
		}
		started[subnetIndex] = listener
		go serveDHCP(subnetIndex, listener)
	}
	for subnetIndex, listener := range m.servers {
		if slices.Contains(desired, subnetIndex) {
			continue
		}
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
			return fmt.Errorf("stop DHCP on %s: %w", bridgeNameForSubnet(subnetIndex), err)
		}
		delete(m.servers, subnetIndex)
	}
	for subnetIndex, listener := range started {
		m.servers[subnetIndex] = listener
	}
	return nil
}

func (m *linuxDHCPManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result error
	for subnetIndex, listener := range m.servers {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
			result = errors.Join(result, fmt.Errorf("stop DHCP on %s: %w", bridgeNameForSubnet(subnetIndex), err))
		}
		delete(m.servers, subnetIndex)
	}
	return result
}

func (m *linuxDHCPManager) handler(subnetIndex int) server4.Handler {
	return func(connection net.PacketConn, peer net.Addr, request *dhcpv4.DHCPv4) {
		clusters, err := m.load()
		if err != nil {
			log.Printf("load DHCP reservations for %s: %v", bridgeNameForSubnet(subnetIndex), err)
			return
		}
		reservations, err := cluster.NewReservationTable(clusters)
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
