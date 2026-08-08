//go:build e2e

package helper

import (
	"encoding/binary"
	"net"
	"time"
)

const (
	arpOpRequest = 1
	arpOpReply   = 2

	icmpTypeEchoReply = 0

	dhcpOpReply = 2

	dhcpMessageDiscover = 1
	dhcpMessageOffer    = 2
	dhcpMessageACK      = 5
	dhcpMessageNACK     = 6
)

type dhcpMessage struct {
	XID       uint32
	Message   byte
	YourIP    net.IP
	ServerIP  net.IP
	ClientMAC net.HardwareAddr
}

func leaseInSubnet(ip net.IP, subnet int) bool {
	ip = ip.To4()
	return ip != nil &&
		ip[0] == 172 &&
		ip[1] == 30 &&
		int(ip[2]) == subnet &&
		ip[3] >= 2 &&
		ip[3] <= 179
}

func buildARPRequest(srcMAC net.HardwareAddr, srcIP, targetIP net.IP) []byte {
	payload := make([]byte, 28)
	binary.BigEndian.PutUint16(payload[0:2], 1)
	binary.BigEndian.PutUint16(payload[2:4], etherTypeIPv4)
	payload[4] = 6
	payload[5] = 4
	binary.BigEndian.PutUint16(payload[6:8], arpOpRequest)
	copy(payload[8:14], srcMAC)
	copy(payload[14:18], srcIP.To4())
	copy(payload[24:28], targetIP.To4())
	return buildEthernetFrame(
		net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		srcMAC,
		etherTypeARP,
		payload,
	)
}

func buildARPReply(senderIP net.IP, senderMAC, targetMAC net.HardwareAddr, targetIP net.IP) []byte {
	payload := make([]byte, 28)
	binary.BigEndian.PutUint16(payload[0:2], 1)
	binary.BigEndian.PutUint16(payload[2:4], etherTypeIPv4)
	payload[4] = 6
	payload[5] = 4
	binary.BigEndian.PutUint16(payload[6:8], arpOpReply)
	copy(payload[8:14], senderMAC)
	copy(payload[14:18], senderIP.To4())
	copy(payload[18:24], targetMAC)
	copy(payload[24:28], targetIP.To4())
	return buildEthernetFrame(targetMAC, senderMAC, etherTypeARP, payload)
}

func buildICMPEchoFrame(srcMAC, dstMAC net.HardwareAddr, srcIP, dstIP net.IP, id, seq uint16, payload []byte) []byte {
	return buildICMPEchoFrameWithTTL(srcMAC, dstMAC, srcIP, dstIP, id, seq, payload, 64)
}

func buildICMPReplyFrame(srcMAC, dstMAC net.HardwareAddr, srcIP, dstIP net.IP, id, seq uint16, payload []byte) []byte {
	icmp := make([]byte, 8+len(payload))
	icmp[0] = icmpTypeEchoReply
	binary.BigEndian.PutUint16(icmp[4:6], id)
	binary.BigEndian.PutUint16(icmp[6:8], seq)
	copy(icmp[8:], payload)
	binary.BigEndian.PutUint16(icmp[2:4], checksum(icmp))
	return buildIPv4EthernetFrame(dstMAC, srcMAC, srcIP, dstIP, ipProtocolICMP, icmp)
}

func parseARPPacket(payload []byte) (op uint16, senderMAC net.HardwareAddr, senderIP net.IP, targetMAC net.HardwareAddr, targetIP net.IP, ok bool) {
	if len(payload) < 28 {
		return 0, nil, nil, nil, nil, false
	}
	if binary.BigEndian.Uint16(payload[0:2]) != 1 || binary.BigEndian.Uint16(payload[2:4]) != etherTypeIPv4 || payload[4] != 6 || payload[5] != 4 {
		return 0, nil, nil, nil, nil, false
	}
	return binary.BigEndian.Uint16(payload[6:8]),
		copyMAC(payload[8:14]),
		append(net.IP(nil), payload[14:18]...),
		copyMAC(payload[18:24]),
		append(net.IP(nil), payload[24:28]...),
		true
}

func parseIPv4Packet(payload []byte) (protocol byte, srcIP, dstIP net.IP, body []byte, ok bool) {
	header, body, ok := parseIPv4Header(payload)
	if !ok {
		return 0, nil, nil, nil, false
	}
	return header[9], append(net.IP(nil), header[12:16]...), append(net.IP(nil), header[16:20]...), body, true
}

func parseUDPSegment(payload []byte) (srcPort, dstPort uint16, body []byte, ok bool) {
	if len(payload) < 8 {
		return 0, 0, nil, false
	}
	length := int(binary.BigEndian.Uint16(payload[4:6]))
	if length < 8 || length > len(payload) {
		return 0, 0, nil, false
	}
	return binary.BigEndian.Uint16(payload[0:2]), binary.BigEndian.Uint16(payload[2:4]), append([]byte(nil), payload[8:length]...), true
}

func parseDHCPMessage(payload []byte) (dhcpMessage, bool) {
	if len(payload) < 240 || payload[0] != dhcpOpReply || payload[1] != 1 || payload[2] != 6 {
		return dhcpMessage{}, false
	}
	if binary.BigEndian.Uint32(payload[236:240]) != dhcpMagicCookie {
		return dhcpMessage{}, false
	}

	msg := dhcpMessage{
		XID:       binary.BigEndian.Uint32(payload[4:8]),
		YourIP:    append(net.IP(nil), payload[16:20]...),
		ClientMAC: copyMAC(payload[28:34]),
	}
	for i := 240; i < len(payload); {
		option := payload[i]
		i++
		switch option {
		case 0:
			continue
		case dhcpOptionEnd:
			return msg, true
		}
		if i >= len(payload) {
			return dhcpMessage{}, false
		}
		length := int(payload[i])
		i++
		if i+length > len(payload) {
			return dhcpMessage{}, false
		}
		value := payload[i : i+length]
		switch option {
		case dhcpOptionMessageType:
			if len(value) == 1 {
				msg.Message = value[0]
			}
		case dhcpOptionServerID:
			if len(value) == 4 {
				msg.ServerIP = append(net.IP(nil), value...)
			}
		}
		i += length
	}
	return msg, true
}

func parseICMPEcho(payload []byte) (icmpType byte, id, seq uint16, body []byte, ok bool) {
	if len(payload) < 8 {
		return 0, 0, 0, nil, false
	}
	return payload[0], binary.BigEndian.Uint16(payload[4:6]), binary.BigEndian.Uint16(payload[6:8]), append([]byte(nil), payload[8:]...), true
}

func sameSubnet(a, b net.IP) bool {
	a4 := a.To4()
	b4 := b.To4()
	return a4 != nil && b4 != nil && a4[0] == b4[0] && a4[1] == b4[1] && a4[2] == b4[2]
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
