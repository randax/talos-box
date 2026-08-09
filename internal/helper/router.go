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
	ipToPort map[routerIP]*routerPort
}

type routerPort struct {
	id         int
	subnet     int
	sendMu     sync.RWMutex
	send       func([]byte) error
	closed     bool
	mac        routerMAC
	macSet     bool
	gateway    routerMAC
	gatewaySet bool
	nodeIP     routerIP
	nodeIPSet  bool
	ips        map[routerIP]struct{}
}

type routerIP [4]byte

type routerMAC [6]byte

func newFrameRouter() *frameRouter {
	return &frameRouter{
		ports:    make(map[int]*routerPort),
		ipToPort: make(map[routerIP]*routerPort),
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
		ips:    make(map[routerIP]struct{}),
	}
	r.ports[port.id] = port
	return port
}

func (r *frameRouter) removePort(port *routerPort) {
	if port == nil {
		return
	}

	r.mu.Lock()

	current, ok := r.ports[port.id]
	if !ok || current != port {
		r.mu.Unlock()
		return
	}
	delete(r.ports, port.id)
	for ip := range port.ips {
		if owner := r.ipToPort[ip]; owner == port {
			delete(r.ipToPort, ip)
		}
	}
	r.mu.Unlock()

	port.closeSend()
}

func (r *frameRouter) route(port *routerPort, frame []byte) (bool, error) {
	dstMAC, srcMAC, etherType, payload, ok := parseRouterEthernetFrame(frame)
	if !ok {
		return false, nil
	}
	if etherType != routerEtherTypeARP && etherType != routerEtherTypeIPv4 {
		return false, nil
	}

	var (
		headerLen  int
		ttl        byte
		protocol   byte
		fragmented bool
		srcIP      net.IP
		dstIP      net.IP
		l4Payload  []byte
	)
	if etherType == routerEtherTypeIPv4 {
		headerLen, ttl, protocol, fragmented, srcIP, dstIP, l4Payload, ok = parseRouterIPv4Packet(payload)
		if !ok {
			return false, nil
		}
	}

	r.mu.Lock()
	if port == nil || r.ports[port.id] != port || !r.acceptPortSourceMAC(port, srcMAC) {
		r.mu.Unlock()
		return false, nil
	}
	if etherType == routerEtherTypeARP {
		r.learnARPSender(port, srcMAC, payload)
		r.mu.Unlock()
		return false, nil
	}

	if protocol == routerIPProtocolUDP && !fragmented {
		r.learnDHCPRequestedIP(port, srcMAC, l4Payload)
	}

	srcKey, ok := routerIPKey(srcIP)
	if !ok || r.ipToPort[srcKey] != port {
		r.mu.Unlock()
		return false, nil
	}
	dstSubnet, ok := talosboxSubnet(dstIP)
	if !ok || dstSubnet == port.subnet || !isOwnedTalosboxIP(dstIP, dstSubnet) {
		r.mu.Unlock()
		return false, nil
	}
	dstKey, ok := routerIPKey(dstIP)
	if !ok {
		r.mu.Unlock()
		return false, nil
	}

	if isUnicastMAC(dstMAC) {
		port.gateway = dstMAC
		port.gatewaySet = true
	}

	target := r.targetFor(dstKey, dstSubnet)
	if target == nil {
		r.mu.Unlock()
		return false, nil
	}
	if ttl <= 1 {
		r.mu.Unlock()
		return true, nil
	}
	targetMAC := target.mac
	sourceMAC := target.sourceMAC()
	targetSubnet := target.subnet
	r.mu.Unlock()

	forwarded := append([]byte(nil), frame...)
	copy(forwarded[0:6], targetMAC[:])
	copy(forwarded[6:12], sourceMAC[:])
	forwarded[routerEthernetHeaderLen+8] = ttl - 1
	header := forwarded[routerEthernetHeaderLen : routerEthernetHeaderLen+headerLen]
	header[10] = 0
	header[11] = 0
	binary.BigEndian.PutUint16(header[10:12], routerInternetChecksum(header))
	if err := target.sendFrame(forwarded); err != nil {
		return true, fmt.Errorf("forward %s -> %s via subnet %d: %w", srcIP, dstIP, targetSubnet, err)
	}
	return true, nil
}

func (r *frameRouter) acceptPortSourceMAC(port *routerPort, mac routerMAC) bool {
	if !isUnicastMAC(mac) {
		return false
	}
	if !port.macSet {
		port.mac = mac
		port.macSet = true
		return true
	}
	return port.mac == mac
}

func (r *frameRouter) learnARPSender(port *routerPort, frameSrcMAC routerMAC, payload []byte) {
	op, senderMAC, senderIP, targetIP, ok := parseRouterARP(payload)
	if !ok {
		return
	}
	if senderMAC != frameSrcMAC || !isVIP(senderIP, port.subnet) {
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

func (r *frameRouter) learnDHCPRequestedIP(port *routerPort, frameSrcMAC routerMAC, payload []byte) {
	srcPort, dstPort, body, ok := parseRouterUDP(payload)
	if !ok || srcPort != routerDHCPClientPort || dstPort != routerDHCPServerPort {
		return
	}
	requestedIP, serverIP, clientMAC, messageType, ok := parseRouterDHCPRequest(body)
	if !ok ||
		messageType != routerDHCPMessageRequest ||
		clientMAC != frameSrcMAC ||
		clientMAC != port.mac ||
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
	key, ok := routerIPKey(ip4)
	if !ok {
		return
	}
	if port.nodeIPSet && port.nodeIP == key {
		return
	}
	if owner := r.ipToPort[key]; owner != nil && owner != port {
		return
	}
	if port.nodeIPSet {
		if owner := r.ipToPort[port.nodeIP]; owner == port {
			delete(r.ipToPort, port.nodeIP)
		}
		delete(port.ips, port.nodeIP)
	}
	port.nodeIP = key
	port.nodeIPSet = true
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
	key, ok := routerIPKey(ip4)
	if !ok {
		return
	}
	if owner := r.ipToPort[key]; owner != nil && owner != port {
		delete(owner.ips, key)
	}
	port.ips[key] = struct{}{}
	r.ipToPort[key] = port
}

func (r *frameRouter) targetFor(ip routerIP, subnet int) *routerPort {
	if target := r.ipToPort[ip]; target != nil && target.subnet == subnet && target.macSet {
		return target
	}
	return nil
}

func (p *routerPort) sourceMAC() routerMAC {
	if p.gatewaySet {
		return p.gateway
	}
	return routerGatewayMACKey(p.subnet)
}

func (p *routerPort) sendFrame(frame []byte) error {
	p.sendMu.RLock()
	defer p.sendMu.RUnlock()

	if p.closed {
		return fmt.Errorf("router port %d is closed", p.id)
	}
	return p.send(frame)
}

func (p *routerPort) closeSend() {
	p.sendMu.Lock()
	p.closed = true
	p.sendMu.Unlock()
}

func routerGatewayMAC(subnet int) net.HardwareAddr {
	mac := routerGatewayMACKey(subnet)
	return net.HardwareAddr(mac[:])
}

func routerGatewayMACKey(subnet int) routerMAC {
	return routerMAC{0x02, 0x54, 0x30, 0x00, byte(subnet), 0x01}
}

func parseRouterEthernetFrame(frame []byte) (dst, src routerMAC, etherType uint16, payload []byte, ok bool) {
	if len(frame) < routerEthernetHeaderLen {
		return routerMAC{}, routerMAC{}, 0, nil, false
	}
	copy(dst[:], frame[0:6])
	copy(src[:], frame[6:12])
	return dst, src, binary.BigEndian.Uint16(frame[12:14]), frame[14:], true
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

func parseRouterARP(payload []byte) (op uint16, senderMAC routerMAC, senderIP, targetIP net.IP, ok bool) {
	if len(payload) < 28 {
		return 0, routerMAC{}, nil, nil, false
	}
	if binary.BigEndian.Uint16(payload[0:2]) != 1 ||
		binary.BigEndian.Uint16(payload[2:4]) != routerEtherTypeIPv4 ||
		payload[4] != 6 ||
		payload[5] != 4 {
		return 0, routerMAC{}, nil, nil, false
	}
	copy(senderMAC[:], payload[8:14])
	return binary.BigEndian.Uint16(payload[6:8]),
		senderMAC,
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

func parseRouterDHCPRequest(payload []byte) (requestedIP, serverIP net.IP, clientMAC routerMAC, messageType byte, ok bool) {
	if len(payload) < 240 || payload[0] != 1 || payload[1] != 1 || payload[2] != 6 {
		return nil, nil, routerMAC{}, 0, false
	}
	if binary.BigEndian.Uint32(payload[236:240]) != routerDHCPMagicCookie {
		return nil, nil, routerMAC{}, 0, false
	}
	copy(clientMAC[:], payload[28:34])
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
			return nil, nil, routerMAC{}, 0, false
		}
		length := int(payload[i])
		i++
		if i+length > len(payload) {
			return nil, nil, routerMAC{}, 0, false
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

func routerIPKey(ip net.IP) (routerIP, bool) {
	ip4 := ip.To4()
	if ip4 == nil {
		return routerIP{}, false
	}
	return routerIP{ip4[0], ip4[1], ip4[2], ip4[3]}, true
}

func isUnicastMAC(mac routerMAC) bool {
	return mac[0]&1 == 0
}

func routerDHCPServerIDMatchesSubnet(serverIP net.IP, subnet int) bool {
	ip4 := serverIP.To4()
	return ip4 != nil && ip4[0] == 172 && ip4[1] == 30 && int(ip4[2]) == subnet && ip4[3] == 1
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
