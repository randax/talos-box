package helper

import (
	"encoding/binary"
	"net"
	"testing"
)

const (
	etherTypeIPv4 = 0x0800
	etherTypeARP  = 0x0806

	ipProtocolICMP = 1
	ipProtocolUDP  = 17

	icmpTypeEchoRequest = 8

	dhcpClientPort = 68
	dhcpServerPort = 67

	dhcpOpRequest = 1

	dhcpMessageRequest = 3

	dhcpOptionMessageType          = 53
	dhcpOptionRequestedIP          = 50
	dhcpOptionServerID             = 54
	dhcpOptionParameterList        = 55
	dhcpOptionClientID             = 61
	dhcpOptionHostName             = 12
	dhcpOptionEnd                  = 255
	dhcpMagicCookie         uint32 = 0x63825363
)

func mustMAC(t *testing.T, value string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(value)
	if err != nil {
		t.Fatalf("parse MAC %q: %v", value, err)
	}
	return mac
}

func hostIP(subnet, host int) net.IP {
	return net.IPv4(172, 30, byte(subnet), byte(host)).To4()
}

func buildDHCPFrame(mac net.HardwareAddr, xid uint32, messageType byte, requestedIP, serverIP net.IP) []byte {
	options := []byte{
		dhcpOptionMessageType, 1, messageType,
		dhcpOptionParameterList, 5, 1, 3, 6, 51, 54,
		dhcpOptionClientID, 7, 1,
	}
	options = append(options, mac...)
	options = append(options, dhcpOptionHostName, byte(len("tbx-e2e")))
	options = append(options, []byte("tbx-e2e")...)
	if requestedIP != nil && !requestedIP.Equal(net.IPv4zero) {
		options = append(options, dhcpOptionRequestedIP, 4)
		options = append(options, requestedIP.To4()...)
	}
	if serverIP != nil && serverIP.To4() != nil {
		options = append(options, dhcpOptionServerID, 4)
		options = append(options, serverIP.To4()...)
	}
	options = append(options, dhcpOptionEnd)

	bootp := make([]byte, 240)
	bootp[0] = dhcpOpRequest
	bootp[1] = 1
	bootp[2] = 6
	binary.BigEndian.PutUint32(bootp[4:8], xid)
	binary.BigEndian.PutUint16(bootp[10:12], 0x8000)
	copy(bootp[28:34], mac)
	binary.BigEndian.PutUint32(bootp[236:240], dhcpMagicCookie)
	udpPayload := append(bootp, options...)

	return buildUDPIPv4EthernetFrame(
		net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		copyMAC(mac),
		net.IPv4zero,
		net.IPv4bcast,
		dhcpClientPort,
		dhcpServerPort,
		udpPayload,
	)
}

func buildGratuitousARP(senderMAC net.HardwareAddr, senderIP net.IP) []byte {
	payload := make([]byte, 28)
	binary.BigEndian.PutUint16(payload[0:2], 1)
	binary.BigEndian.PutUint16(payload[2:4], etherTypeIPv4)
	payload[4] = 6
	payload[5] = 4
	binary.BigEndian.PutUint16(payload[6:8], 2)
	copy(payload[8:14], senderMAC)
	copy(payload[14:18], senderIP.To4())
	copy(payload[24:28], senderIP.To4())
	return buildEthernetFrame(
		net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		senderMAC,
		etherTypeARP,
		payload,
	)
}

func buildICMPEchoFrameWithTTL(srcMAC, dstMAC net.HardwareAddr, srcIP, dstIP net.IP, id, seq uint16, payload []byte, ttl byte) []byte {
	icmp := make([]byte, 8+len(payload))
	icmp[0] = icmpTypeEchoRequest
	binary.BigEndian.PutUint16(icmp[4:6], id)
	binary.BigEndian.PutUint16(icmp[6:8], seq)
	copy(icmp[8:], payload)
	binary.BigEndian.PutUint16(icmp[2:4], checksum(icmp))
	return buildIPv4EthernetFrameWithTTL(dstMAC, srcMAC, srcIP, dstIP, ttl, ipProtocolICMP, icmp)
}

func buildUDPIPv4EthernetFrame(dstMAC, srcMAC net.HardwareAddr, srcIP, dstIP net.IP, srcPort, dstPort uint16, payload []byte) []byte {
	udp := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], payload)
	return buildIPv4EthernetFrame(dstMAC, srcMAC, srcIP, dstIP, ipProtocolUDP, udp)
}

func buildIPv4EthernetFrame(dstMAC, srcMAC net.HardwareAddr, srcIP, dstIP net.IP, protocol byte, payload []byte) []byte {
	return buildIPv4EthernetFrameWithTTL(dstMAC, srcMAC, srcIP, dstIP, 64, protocol, payload)
}

func buildIPv4EthernetFrameWithTTL(dstMAC, srcMAC net.HardwareAddr, srcIP, dstIP net.IP, ttl, protocol byte, payload []byte) []byte {
	header := make([]byte, 20)
	header[0] = 0x45
	header[8] = ttl
	header[9] = protocol
	binary.BigEndian.PutUint16(header[2:4], uint16(len(header)+len(payload)))
	copy(header[12:16], srcIP.To4())
	copy(header[16:20], dstIP.To4())
	binary.BigEndian.PutUint16(header[10:12], checksum(header))
	return buildEthernetFrame(dstMAC, srcMAC, etherTypeIPv4, append(header, payload...))
}

func buildEthernetFrame(dstMAC, srcMAC net.HardwareAddr, etherType uint16, payload []byte) []byte {
	frame := make([]byte, 14+len(payload))
	copy(frame[0:6], dstMAC)
	copy(frame[6:12], srcMAC)
	binary.BigEndian.PutUint16(frame[12:14], etherType)
	copy(frame[14:], payload)
	return frame
}

func parseEthernetFrame(frame []byte) (dst, src net.HardwareAddr, etherType uint16, payload []byte, ok bool) {
	if len(frame) < 14 {
		return nil, nil, 0, nil, false
	}
	return copyMAC(frame[0:6]), copyMAC(frame[6:12]), binary.BigEndian.Uint16(frame[12:14]), append([]byte(nil), frame[14:]...), true
}

func parseIPv4Header(payload []byte) (header []byte, body []byte, ok bool) {
	if len(payload) < 20 || payload[0]>>4 != 4 {
		return nil, nil, false
	}
	headerLen := int(payload[0]&0x0f) * 4
	if headerLen < 20 || len(payload) < headerLen {
		return nil, nil, false
	}
	totalLen := int(binary.BigEndian.Uint16(payload[2:4]))
	if totalLen < headerLen || totalLen > len(payload) {
		return nil, nil, false
	}
	return append([]byte(nil), payload[:headerLen]...), append([]byte(nil), payload[headerLen:totalLen]...), true
}

func checksum(payload []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(payload); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(payload[i : i+2]))
	}
	if len(payload)%2 == 1 {
		sum += uint32(payload[len(payload)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

func macEqual(a, b net.HardwareAddr) bool {
	return len(a) == len(b) && copyMAC(a).String() == copyMAC(b).String()
}

func copyMAC(value net.HardwareAddr) net.HardwareAddr {
	return append(net.HardwareAddr(nil), value...)
}
