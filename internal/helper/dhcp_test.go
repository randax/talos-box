package helper

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/randax/talos-box/internal/cluster"
)

func TestDHCPReplyEncodesStaticReservationOptions(t *testing.T) {
	t.Parallel()

	table, node := testReservationTable(t)
	discover, err := dhcpv4.NewDiscovery(node.MAC)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := buildDHCPReply(discover, table, 12)
	if err != nil {
		t.Fatal(err)
	}
	if reply == nil {
		t.Fatal("buildDHCPReply() = nil, want offer")
	}
	if reply.MessageType() != dhcpv4.MessageTypeOffer {
		t.Fatalf("message type = %s, want OFFER", reply.MessageType())
	}
	if !reply.YourIPAddr.Equal(net.IPv4(172, 30, 12, 2)) {
		t.Fatalf("yiaddr = %v, want 172.30.12.2", reply.YourIPAddr)
	}
	assertOption(t, reply, dhcpv4.OptionSubnetMask, net.IPMask{255, 255, 255, 0})
	assertOption(t, reply, dhcpv4.OptionRouter, net.IP{172, 30, 12, 1})
	assertOption(t, reply, dhcpv4.OptionDomainNameServer, net.IP{172, 30, 12, 1})
	assertOption(t, reply, dhcpv4.OptionServerIdentifier, net.IP{172, 30, 12, 1})
	lease := reply.Options.Get(dhcpv4.OptionIPAddressLeaseTime)
	if len(lease) != 4 || time.Duration(binary.BigEndian.Uint32(lease))*time.Second != dhcpLeaseDuration {
		t.Fatalf("lease option = %v, want %s", lease, dhcpLeaseDuration)
	}
}

func TestDHCPReplyAcknowledgesOnlyReservedAddress(t *testing.T) {
	t.Parallel()

	table, node := testReservationTable(t)
	discover, err := dhcpv4.NewDiscovery(node.MAC)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := buildDHCPReply(discover, table, 12)
	if err != nil {
		t.Fatal(err)
	}
	request, err := dhcpv4.NewRequestFromOffer(offer)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := buildDHCPReply(request, table, 12)
	if err != nil {
		t.Fatal(err)
	}
	if ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck || !ack.YourIPAddr.Equal(offer.YourIPAddr) {
		t.Fatalf("valid request reply = %#v, want ACK for %v", ack, offer.YourIPAddr)
	}

	request.UpdateOption(dhcpv4.OptRequestedIPAddress(net.IPv4(172, 30, 12, 99)))
	nak, err := buildDHCPReply(request, table, 12)
	if err != nil {
		t.Fatal(err)
	}
	if nak == nil || nak.MessageType() != dhcpv4.MessageTypeNak || !nak.YourIPAddr.Equal(net.IPv4zero) {
		t.Fatalf("wrong-address reply = %#v, want NAK with empty yiaddr", nak)
	}
}

func TestDHCPReplyIgnoresUnknownAndOtherServerRequests(t *testing.T) {
	t.Parallel()

	table, node := testReservationTable(t)
	unknown, err := dhcpv4.NewDiscovery(net.HardwareAddr{0x52, 0x54, 0, 0xff, 0xff, 0xff})
	if err != nil {
		t.Fatal(err)
	}
	if reply, err := buildDHCPReply(unknown, table, 12); err != nil || reply != nil {
		t.Fatalf("unknown client reply = %#v, %v; want nil, nil", reply, err)
	}

	discover, err := dhcpv4.NewDiscovery(node.MAC)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := buildDHCPReply(discover, table, 12)
	if err != nil {
		t.Fatal(err)
	}
	request, err := dhcpv4.NewRequestFromOffer(offer)
	if err != nil {
		t.Fatal(err)
	}
	request.UpdateOption(dhcpv4.OptServerIdentifier(net.IPv4(172, 30, 12, 254)))
	if reply, err := buildDHCPReply(request, table, 12); err != nil || reply != nil {
		t.Fatalf("other-server reply = %#v, %v; want nil, nil", reply, err)
	}
}

func testReservationTable(t *testing.T) (cluster.ReservationTable, cluster.Reservation) {
	t.Helper()
	item, err := cluster.New("dhcp", 12, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	table, err := cluster.NewReservationTable([]cluster.Cluster{item})
	if err != nil {
		t.Fatal(err)
	}
	reservation, ok := table.Lookup(item.Nodes[0].MAC)
	if !ok {
		t.Fatal("reservation missing")
	}
	return table, reservation
}

func assertOption(t *testing.T, reply *dhcpv4.DHCPv4, code dhcpv4.OptionCode, want []byte) {
	t.Helper()
	if got := reply.Options.Get(code); !net.IP(got).Equal(net.IP(want)) {
		t.Fatalf("option %s = %v, want %v", code, got, want)
	}
}
