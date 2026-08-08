package helper

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
)

const (
	routerEthernetHeaderLen = 14
	routerEtherTypeIPv4     = 0x0800
	routerEtherTypeARP      = 0x0806

	routerIPv4MinHeaderLen       = 20
	routerIPProtocolUDP          = 17
	routerIPv4FlagMoreFragments  = 0x2000
	routerIPv4FragmentOffsetMask = 0x1fff

	routerDHCPClientPort = 68
	routerDHCPServerPort = 67

	routerDHCPOptionMessageType = 53
	routerDHCPOptionRequestedIP = 50
	routerDHCPOptionServerID    = 54
	routerDHCPOptionEnd         = 255

	routerDHCPMessageRequest = 3
	routerDHCPMagicCookie    = 0x63825363

	routerARPOpRequest = 1
	routerARPOpReply   = 2
)

type frameRouter struct {
	mu       sync.Mutex
	nextPort int
	ports    map[int]*routerPort
	ipToPort map[string]*routerPort
}

type routerPort struct {
	id      int
	subnet  int
	send    func([]byte) error
	mac     net.HardwareAddr
	gateway net.HardwareAddr
	nodeIP  string
	ips     map[string]struct{}
}

func newFrameRouter() *frameRouter {
	return &frameRouter{
		ports:    make(map[int]*routerPort),
		ipToPort: make(map[string]*routerPort),
	}
}

func (r *frameRouter) addPort(subnet int, send func([]byte) error) *routerPort {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextPort++
	port := &routerPort{
		id:     r.nextPort,
		subnet: subnet,
		send:   send,
		ips:    make(map[string]struct{}),
	}
	r.ports[port.id] = port
	return port
}

func (r *frameRouter) removePort(port *routerPort) {
	if port == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	current, ok := r.ports[port.id]
	if !ok || current != port {
		return
	}
	delete(r.ports, port.id)
	for ip := range port.ips {
		if owner := r.ipToPort[ip]; owner == port {
			delete(r.ipToPort, ip)
		}
	}
}

func (r *frameRouter) route(port *routerPort, frame []byte) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if port == nil || port.send == nil || r.ports[port.id] != port {
		return false, nil
	}

	dstMAC, srcMAC, etherType, payload, ok := parseRouterEthernetFrame(frame)
	if !ok {
		return false, nil
	}
	if !r.acceptPortSourceMAC(port, srcMAC) {
		return false, nil
	}

	switch etherType {
	case routerEtherTypeARP:
		r.learnARPSender(port, srcMAC, payload)
		return false, nil
	case routerEtherTypeIPv4:
	default:
		return false, nil
	}

	headerLen, ttl, protocol, fragmented, srcIP, dstIP, l4Payload, ok := parseRouterIPv4Packet(payload)
	if !ok {
		return false, nil
	}
	if protocol == routerIPProtocolUDP && !fragmented {
		r.learnDHCPRequestedIP(port, srcMAC, l4Payload)
	}

	if owner := r.ipToPort[srcIP.String()]; owner != port {
		return false, nil
	}
	dstSubnet, ok := talosboxSubnet(dstIP)
	if !ok || dstSubnet == port.subnet || !isOwnedTalosboxIP(dstIP, dstSubnet) {
		return false, nil
	}

	if isUnicastMAC(dstMAC) {
		port.gateway = copyRouterMAC(dstMAC)
	}

	target := r.targetFor(dstIP, dstSubnet)
	if target == nil {
		return false, nil
	}
	if ttl <= 1 {
		return true, nil
	}

	forwarded := append([]byte(nil), frame...)
	copy(forwarded[0:6], target.mac)
	copy(forwarded[6:12], target.sourceMAC())
	forwarded[routerEthernetHeaderLen+8] = ttl - 1
	binary.BigEndian.PutUint16(
		forwarded[routerEthernetHeaderLen+10:routerEthernetHeaderLen+12],
		routerIPv4Checksum(forwarded[routerEthernetHeaderLen:routerEthernetHeaderLen+headerLen]),
	)
	if err := target.send(forwarded); err != nil {
		return true, fmt.Errorf("forward %s -> %s via subnet %d: %w", srcIP, dstIP, target.subnet, err)
	}
	return true, nil
}

func (r *frameRouter) acceptPortSourceMAC(port *routerPort, mac net.HardwareAddr) bool {
	if !isUnicastMAC(mac) {
		return false
	}
	if len(port.mac) == 0 {
		port.mac = copyRouterMAC(mac)
		return true
	}
	return routerMACEqual(port.mac, mac)
}

func (r *frameRouter) learnARPSender(port *routerPort, frameSrcMAC net.HardwareAddr, payload []byte) {
	op, senderMAC, senderIP, targetIP, ok := parseRouterARP(payload)
	if !ok {
		return
	}
	if !routerMACEqual(senderMAC, frameSrcMAC) || !isVIP(senderIP, port.subnet) {
		return
	}
	switch op {
	case routerARPOpRequest, routerARPOpReply:
		// Helper-local VIP moves intentionally mirror same-subnet L2 announcement
		// semantics and are not an isolation boundary among nodes on one cluster subnet.
		r.learnVIPIP(port, senderIP)
		_ = targetIP
	}
}

func (r *frameRouter) learnDHCPRequestedIP(port *routerPort, frameSrcMAC net.HardwareAddr, payload []byte) {
	srcPort, dstPort, body, ok := parseRouterUDP(payload)
	if !ok || srcPort != routerDHCPClientPort || dstPort != routerDHCPServerPort {
		return
	}
	requestedIP, serverIP, clientMAC, messageType, ok := parseRouterDHCPRequest(body)
	if !ok ||
		messageType != routerDHCPMessageRequest ||
		!routerMACEqual(clientMAC, frameSrcMAC) ||
		!routerMACEqual(clientMAC, port.mac) ||
		!isNodeIP(requestedIP, port.subnet) ||
		!routerDHCPServerIDMatchesSubnet(serverIP, port.subnet) {
		return
	}
	// vmnet DHCP replies travel directly vmnet->guest and bypass the Go pump.
	// Helper attachments inside one cluster subnet therefore share a single L2
	// trust domain, and a validated DHCPREQUEST is the observable lease evidence
	// rather than a tenant-isolation boundary.
	r.learnNodeIP(port, requestedIP)
}

func (r *frameRouter) learnNodeIP(port *routerPort, ip net.IP) {
	if port == nil {
		return
	}
	ip4 := ip.To4()
	if !isNodeIP(ip4, port.subnet) {
		return
	}
	key := ip4.String()
	if port.nodeIP == key {
		return
	}
	if owner := r.ipToPort[key]; owner != nil && owner != port {
		return
	}
	if port.nodeIP != "" {
		if owner := r.ipToPort[port.nodeIP]; owner == port {
			delete(r.ipToPort, port.nodeIP)
		}
		delete(port.ips, port.nodeIP)
	}
	port.nodeIP = key
	port.ips[key] = struct{}{}
	r.ipToPort[key] = port
}

func (r *frameRouter) learnVIPIP(port *routerPort, ip net.IP) {
	if port == nil {
		return
	}
	ip4 := ip.To4()
	if !isVIP(ip4, port.subnet) {
		return
	}
	key := ip4.String()
	if owner := r.ipToPort[key]; owner != nil && owner != port {
		delete(owner.ips, key)
	}
	port.ips[key] = struct{}{}
	r.ipToPort[key] = port
}

func (r *frameRouter) targetFor(ip net.IP, subnet int) *routerPort {
	if target := r.ipToPort[ip.String()]; target != nil && target.subnet == subnet && len(target.mac) == 6 {
		return target
	}
	return nil
}

func (p *routerPort) sourceMAC() net.HardwareAddr {
	if len(p.gateway) == 6 {
		return copyRouterMAC(p.gateway)
	}
	return routerGatewayMAC(p.subnet)
}

func routerGatewayMAC(subnet int) net.HardwareAddr {
	return net.HardwareAddr{0x02, 0x54, 0x30, 0x00, byte(subnet), 0x01}
}

func parseRouterEthernetFrame(frame []byte) (dst, src net.HardwareAddr, etherType uint16, payload []byte, ok bool) {
	if len(frame) < routerEthernetHeaderLen {
		return nil, nil, 0, nil, false
	}
	return copyRouterMAC(frame[0:6]),
		copyRouterMAC(frame[6:12]),
		binary.BigEndian.Uint16(frame[12:14]),
		frame[14:],
		true
}

func parseRouterIPv4Packet(payload []byte) (headerLen int, ttl, protocol byte, fragmented bool, srcIP, dstIP net.IP, l4Payload []byte, ok bool) {
	if len(payload) < routerIPv4MinHeaderLen || payload[0]>>4 != 4 {
		return 0, 0, 0, false, nil, nil, nil, false
	}
	headerLen = int(payload[0]&0x0f) * 4
	if headerLen < routerIPv4MinHeaderLen || len(payload) < headerLen {
		return 0, 0, 0, false, nil, nil, nil, false
	}
	totalLen := int(binary.BigEndian.Uint16(payload[2:4]))
	if totalLen < headerLen || totalLen > len(payload) {
		return 0, 0, 0, false, nil, nil, nil, false
	}
	header := payload[:headerLen]
	if routerInternetChecksum(header) != 0 {
		return 0, 0, 0, false, nil, nil, nil, false
	}
	flagsOffset := binary.BigEndian.Uint16(payload[6:8])
	fragmented = (flagsOffset&routerIPv4FlagMoreFragments) != 0 || (flagsOffset&routerIPv4FragmentOffsetMask) != 0
	return headerLen,
		payload[8],
		payload[9],
		fragmented,
		net.IP(payload[12:16]).To4(),
		net.IP(payload[16:20]).To4(),
		payload[headerLen:totalLen],
		true
}

func parseRouterARP(payload []byte) (op uint16, senderMAC net.HardwareAddr, senderIP, targetIP net.IP, ok bool) {
	if len(payload) < 28 {
		return 0, nil, nil, nil, false
	}
	if binary.BigEndian.Uint16(payload[0:2]) != 1 ||
		binary.BigEndian.Uint16(payload[2:4]) != routerEtherTypeIPv4 ||
		payload[4] != 6 ||
		payload[5] != 4 {
		return 0, nil, nil, nil, false
	}
	return binary.BigEndian.Uint16(payload[6:8]),
		copyRouterMAC(payload[8:14]),
		net.IP(payload[14:18]).To4(),
		net.IP(payload[24:28]).To4(),
		true
}

func parseRouterUDP(payload []byte) (srcPort, dstPort uint16, body []byte, ok bool) {
	if len(payload) < 8 {
		return 0, 0, nil, false
	}
	length := int(binary.BigEndian.Uint16(payload[4:6]))
	if length < 8 || length > len(payload) {
		return 0, 0, nil, false
	}
	return binary.BigEndian.Uint16(payload[0:2]),
		binary.BigEndian.Uint16(payload[2:4]),
		payload[8:length],
		true
}

func parseRouterDHCPRequest(payload []byte) (requestedIP, serverIP net.IP, clientMAC net.HardwareAddr, messageType byte, ok bool) {
	if len(payload) < 240 || payload[0] != 1 || payload[1] != 1 || payload[2] != 6 {
		return nil, nil, nil, 0, false
	}
	if binary.BigEndian.Uint32(payload[236:240]) != routerDHCPMagicCookie {
		return nil, nil, nil, 0, false
	}
	clientMAC = copyRouterMAC(payload[28:34])
	for i := 240; i < len(payload); {
		option := payload[i]
		i++
		switch option {
		case 0:
			continue
		case routerDHCPOptionEnd:
			return requestedIP, serverIP, clientMAC, messageType, requestedIP != nil
		}
		if i >= len(payload) {
			return nil, nil, nil, 0, false
		}
		length := int(payload[i])
		i++
		if i+length > len(payload) {
			return nil, nil, nil, 0, false
		}
		value := payload[i : i+length]
		switch option {
		case routerDHCPOptionMessageType:
			if len(value) == 1 {
				messageType = value[0]
			}
		case routerDHCPOptionRequestedIP:
			if len(value) == 4 {
				requestedIP = net.IP(value).To4()
			}
		case routerDHCPOptionServerID:
			if len(value) == 4 {
				serverIP = net.IP(value).To4()
			}
		}
		i += length
	}
	return requestedIP, serverIP, clientMAC, messageType, requestedIP != nil
}

func talosboxSubnet(ip net.IP) (int, bool) {
	ip4 := ip.To4()
	if ip4 == nil || ip4[0] != 172 || ip4[1] != 30 {
		return 0, false
	}
	return int(ip4[2]), true
}

func belongsToSubnet(ip net.IP, subnet int) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 172 && ip4[1] == 30 && int(ip4[2]) == subnet
}

func isOwnedTalosboxIP(ip net.IP, subnet int) bool {
	return isNodeIP(ip, subnet) || isVIP(ip, subnet)
}

func isNodeIP(ip net.IP, subnet int) bool {
	ip4 := ip.To4()
	if !belongsToSubnet(ip4, subnet) {
		return false
	}
	return ip4[3] >= 2 && ip4[3] <= 179
}

func isVIP(ip net.IP, subnet int) bool {
	ip4 := ip.To4()
	if !belongsToSubnet(ip4, subnet) {
		return false
	}
	return ip4[3] >= 200 && ip4[3] <= 239
}

func isUnicastMAC(mac net.HardwareAddr) bool {
	return len(mac) == 6 && mac[0]&1 == 0
}

func copyRouterMAC(value net.HardwareAddr) net.HardwareAddr {
	return append(net.HardwareAddr(nil), value...)
}

func routerMACEqual(a, b net.HardwareAddr) bool {
	return len(a) == len(b) && copyRouterMAC(a).String() == copyRouterMAC(b).String()
}

func routerDHCPServerIDMatchesSubnet(serverIP net.IP, subnet int) bool {
	ip4 := serverIP.To4()
	return ip4 != nil && ip4[0] == 172 && ip4[1] == 30 && int(ip4[2]) == subnet && ip4[3] == 1
}

func routerIPv4Checksum(header []byte) uint16 {
	copyHeader := append([]byte(nil), header...)
	copy(copyHeader[10:12], []byte{0, 0})
	return routerInternetChecksum(copyHeader)
}

func routerInternetChecksum(payload []byte) uint16 {
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
