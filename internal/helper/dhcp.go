package helper

import (
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/randax/talos-box/internal/cluster"
)

const dhcpLeaseDuration = 24 * time.Hour

func buildDHCPReply(request *dhcpv4.DHCPv4, reservations cluster.ReservationTable, subnetIndex int) (*dhcpv4.DHCPv4, error) {
	reservation, ok := reservations.Lookup(request.ClientHWAddr.String())
	if !ok || reservation.SubnetIndex != subnetIndex {
		return nil, nil
	}

	gateway := net.ParseIP(cluster.Gateway(subnetIndex)).To4()
	var messageType dhcpv4.MessageType
	switch request.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		messageType = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		if serverID := request.ServerIdentifier(); serverID != nil && !serverID.Equal(gateway) {
			return nil, nil
		}
		requestedIP := request.RequestedIPAddress()
		if requestedIP == nil || requestedIP.Equal(net.IPv4zero) {
			requestedIP = request.ClientIPAddr
		}
		if !requestedIP.Equal(reservation.IP) {
			return newDHCPReply(request, dhcpv4.MessageTypeNak, net.IPv4zero, gateway)
		}
		messageType = dhcpv4.MessageTypeAck
	default:
		return nil, nil
	}

	return newDHCPReply(request, messageType, reservation.IP, gateway)
}

func newDHCPReply(request *dhcpv4.DHCPv4, messageType dhcpv4.MessageType, assignedIP, gateway net.IP) (*dhcpv4.DHCPv4, error) {
	reply, err := dhcpv4.NewReplyFromRequest(
		request,
		dhcpv4.WithMessageType(messageType),
		dhcpv4.WithYourIP(assignedIP),
		dhcpv4.WithServerIP(gateway),
	)
	if err != nil {
		return nil, err
	}
	reply.UpdateOption(dhcpv4.OptServerIdentifier(gateway))
	if messageType == dhcpv4.MessageTypeNak {
		return reply, nil
	}
	reply.UpdateOption(dhcpv4.OptSubnetMask(net.CIDRMask(24, 32)))
	reply.UpdateOption(dhcpv4.OptRouter(gateway))
	reply.UpdateOption(dhcpv4.OptDNS(gateway))
	reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(dhcpLeaseDuration))
	return reply, nil
}
