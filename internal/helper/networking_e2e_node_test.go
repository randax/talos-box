//go:build e2e

package helper

import (
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type syntheticNode struct {
	t       *testing.T
	cluster string
	name    string
	subnet  int
	mac     net.HardwareAddr
	conn    *net.UnixConn

	ip      net.IP
	vip     net.IP
	gateway net.IP

	writeMu sync.Mutex

	stateMu sync.RWMutex

	dhcpMu      sync.Mutex
	dhcpWaiters map[uint32]chan dhcpMessage

	arpMu      sync.Mutex
	arpCache   map[string]net.HardwareAddr
	arpWaiters map[string][]chan net.HardwareAddr

	icmpMu      sync.Mutex
	icmpWaiters map[icmpWaitKey][]chan struct{}

	closeOnce sync.Once
}

type icmpWaitKey struct {
	src string
	id  uint16
	seq uint16
}

func newSyntheticNode(t *testing.T, cluster, name string, subnet int, mac net.HardwareAddr, conn *net.UnixConn) *syntheticNode {
	t.Helper()
	return &syntheticNode{
		t:           t,
		cluster:     cluster,
		name:        name,
		subnet:      subnet,
		mac:         copyMAC(mac),
		conn:        conn,
		gateway:     hostIP(subnet, 1),
		dhcpWaiters: make(map[uint32]chan dhcpMessage),
		arpCache:    make(map[string]net.HardwareAddr),
		arpWaiters:  make(map[string][]chan net.HardwareAddr),
		icmpWaiters: make(map[icmpWaitKey][]chan struct{}),
	}
}

func (n *syntheticNode) acquireDHCPLease() net.IP {
	n.t.Helper()

	xid := uint32(0x22000000 | uint32(n.subnet))
	waiter := make(chan dhcpMessage, 8)
	n.dhcpMu.Lock()
	n.dhcpWaiters[xid] = waiter
	n.dhcpMu.Unlock()
	defer func() {
		n.dhcpMu.Lock()
		delete(n.dhcpWaiters, xid)
		n.dhcpMu.Unlock()
	}()

	deadline := time.Now().Add(networkProbeTimeout)
	discover := buildDHCPFrame(n.mac, xid, dhcpMessageDiscover, net.IPv4zero, nil)
	var offer dhcpMessage
	for {
		if err := n.writeFrame(discover); err != nil {
			n.t.Fatalf("write DHCP discover on subnet %d: %v", n.subnet, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			n.t.Fatalf("timed out waiting for DHCP offer on subnet %d", n.subnet)
		}
		select {
		case offer = <-waiter:
			if offer.Message != dhcpMessageOffer {
				continue
			}
			if !leaseInSubnet(offer.YourIP, n.subnet) {
				n.t.Fatalf("DHCP offer %s is outside subnet %d", offer.YourIP, n.subnet)
			}
			goto haveOffer
		case <-time.After(minDuration(requestRetryDelay, remaining)):
		}
	}

haveOffer:
	request := buildDHCPFrame(n.mac, xid, dhcpMessageRequest, offer.YourIP, offer.ServerIP)
	for {
		if err := n.writeFrame(request); err != nil {
			n.t.Fatalf("write DHCP request on subnet %d: %v", n.subnet, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			n.t.Fatalf("timed out waiting for DHCP ack on subnet %d", n.subnet)
		}
		select {
		case ack := <-waiter:
			switch ack.Message {
			case dhcpMessageACK:
				if !leaseInSubnet(ack.YourIP, n.subnet) {
					n.t.Fatalf("DHCP ack %s is outside subnet %d", ack.YourIP, n.subnet)
				}
				n.stateMu.Lock()
				n.ip = ack.YourIP.To4()
				n.stateMu.Unlock()
				return ack.YourIP
			case dhcpMessageNACK:
				n.t.Fatalf("received DHCP NACK on subnet %d", n.subnet)
			}
		case <-time.After(minDuration(requestRetryDelay, remaining)):
		}
	}
}

func (n *syntheticNode) claimVIP(ip net.IP) {
	n.t.Helper()

	n.stateMu.Lock()
	n.vip = ip.To4()
	n.stateMu.Unlock()
	for i := 0; i < 3; i++ {
		_ = n.writeFrame(buildGratuitousARP(n.mac, ip))
		time.Sleep(150 * time.Millisecond)
	}
}

func (n *syntheticNode) pingIP(dst net.IP) {
	n.t.Helper()

	src := n.nodeIP()
	if src == nil {
		n.t.Fatalf("node %s has no DHCP lease", n.name)
	}

	via := dst
	if !sameSubnet(src, dst) {
		via = n.gateway
	}
	mac := n.resolveARP(via)
	id := uint16(0x2200 + n.subnet)
	seq := uint16(dst[3])
	waiter := make(chan struct{}, 1)
	key := icmpWaitKey{src: dst.String(), id: id, seq: seq}
	n.icmpMu.Lock()
	n.icmpWaiters[key] = append(n.icmpWaiters[key], waiter)
	n.icmpMu.Unlock()
	defer n.unregisterICMPWaiter(key, waiter)

	frame := buildICMPEchoFrame(n.mac, mac, src, dst, id, seq, []byte("talos-box-issue-22"))
	deadline := time.Now().Add(networkProbeTimeout)
	for {
		if err := n.writeFrame(frame); err != nil {
			n.t.Fatalf("send ping %s -> %s: %v", src, dst, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			n.t.Fatalf("timed out waiting for ping reply %s -> %s", src, dst)
		}
		select {
		case <-waiter:
			n.t.Logf("ping %s -> %s succeeded", src, dst)
			return
		case <-time.After(minDuration(requestRetryDelay, remaining)):
		}
	}
}

func (n *syntheticNode) resolveARP(ip net.IP) net.HardwareAddr {
	n.t.Helper()

	key := ip.String()
	n.arpMu.Lock()
	if mac, ok := n.arpCache[key]; ok {
		n.arpMu.Unlock()
		return copyMAC(mac)
	}
	waiter := make(chan net.HardwareAddr, 4)
	n.arpWaiters[key] = append(n.arpWaiters[key], waiter)
	n.arpMu.Unlock()
	defer n.unregisterARPWaiter(key, waiter)

	frame := buildARPRequest(n.mac, n.nodeIP(), ip)
	deadline := time.Now().Add(networkProbeTimeout)
	for {
		if err := n.writeFrame(frame); err != nil {
			n.t.Fatalf("send ARP for %s: %v", ip, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			n.t.Fatalf("timed out resolving ARP for %s", ip)
		}
		select {
		case mac := <-waiter:
			return mac
		case <-time.After(minDuration(requestRetryDelay, remaining)):
		}
	}
}

func (n *syntheticNode) unregisterARPWaiter(key string, waiter chan net.HardwareAddr) {
	n.arpMu.Lock()
	defer n.arpMu.Unlock()
	waiters := n.arpWaiters[key]
	for i, current := range waiters {
		if current == waiter {
			n.arpWaiters[key] = append(waiters[:i], waiters[i+1:]...)
			if len(n.arpWaiters[key]) == 0 {
				delete(n.arpWaiters, key)
			}
			return
		}
	}
}

func (n *syntheticNode) unregisterICMPWaiter(key icmpWaitKey, waiter chan struct{}) {
	n.icmpMu.Lock()
	defer n.icmpMu.Unlock()
	waiters := n.icmpWaiters[key]
	for i, current := range waiters {
		if current == waiter {
			n.icmpWaiters[key] = append(waiters[:i], waiters[i+1:]...)
			if len(n.icmpWaiters[key]) == 0 {
				delete(n.icmpWaiters, key)
			}
			return
		}
	}
}

func (n *syntheticNode) nodeIP() net.IP {
	n.stateMu.RLock()
	defer n.stateMu.RUnlock()
	if n.ip == nil {
		return nil
	}
	return append(net.IP(nil), n.ip...)
}

func (n *syntheticNode) vipIP() net.IP {
	n.stateMu.RLock()
	defer n.stateMu.RUnlock()
	if n.vip == nil {
		return nil
	}
	return append(net.IP(nil), n.vip...)
}

func (n *syntheticNode) close() {
	n.closeOnce.Do(func() {
		_ = n.conn.Close()
	})
}

func (n *syntheticNode) readLoop() {
	buffer := make([]byte, frameReadSize)
	for {
		size, err := n.conn.Read(buffer)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "use of closed network connection") {
				n.t.Logf("synthetic node %s read stopped: %v", n.name, err)
			}
			return
		}
		frame := append([]byte(nil), buffer[:size]...)
		n.handleFrame(frame)
	}
}

func (n *syntheticNode) handleFrame(frame []byte) {
	dst, src, etherType, payload, ok := parseEthernetFrame(frame)
	if !ok {
		return
	}

	switch etherType {
	case etherTypeARP:
		n.handleARP(src, payload)
	case etherTypeIPv4:
		n.handleIPv4(dst, src, payload)
	}
}

func (n *syntheticNode) handleARP(frameSrc net.HardwareAddr, payload []byte) {
	op, senderMAC, senderIP, _, targetIP, ok := parseARPPacket(payload)
	if !ok {
		return
	}

	if op == arpOpReply {
		n.rememberARP(senderIP, senderMAC)
		return
	}
	if op != arpOpRequest {
		return
	}

	if target := n.replyAddressFor(targetIP); target != nil {
		reply := buildARPReply(target, n.mac, senderMAC, senderIP)
		_ = frameSrc
		_ = n.writeFrame(reply)
	}
}

func (n *syntheticNode) handleIPv4(frameDst, frameSrc net.HardwareAddr, payload []byte) {
	protocol, srcIP, dstIP, body, ok := parseIPv4Packet(payload)
	if !ok {
		return
	}

	switch protocol {
	case ipProtocolUDP:
		srcPort, dstPort, udpPayload, ok := parseUDPSegment(body)
		if !ok || srcPort != dhcpServerPort || dstPort != dhcpClientPort {
			return
		}
		msg, ok := parseDHCPMessage(udpPayload)
		if !ok || !macEqual(msg.ClientMAC, n.mac) {
			return
		}
		n.dhcpMu.Lock()
		waiter := n.dhcpWaiters[msg.XID]
		n.dhcpMu.Unlock()
		if waiter != nil {
			select {
			case waiter <- msg:
			default:
			}
		}
	case ipProtocolICMP:
		icmpType, id, seq, icmpPayload, ok := parseICMPEcho(body)
		if !ok {
			return
		}
		switch icmpType {
		case icmpTypeEchoRequest:
			if replySrc := n.replyAddressFor(dstIP); replySrc != nil {
				reply := buildICMPReplyFrame(n.mac, frameSrc, replySrc, srcIP, id, seq, icmpPayload)
				_ = frameDst
				_ = n.writeFrame(reply)
			}
		case icmpTypeEchoReply:
			key := icmpWaitKey{src: srcIP.String(), id: id, seq: seq}
			n.icmpMu.Lock()
			waiters := append([]chan struct{}(nil), n.icmpWaiters[key]...)
			n.icmpMu.Unlock()
			for _, waiter := range waiters {
				select {
				case waiter <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (n *syntheticNode) replyAddressFor(ip net.IP) net.IP {
	if nodeIP := n.nodeIP(); nodeIP != nil && nodeIP.Equal(ip) {
		return nodeIP
	}
	if vip := n.vipIP(); vip != nil && vip.Equal(ip) {
		return vip
	}
	return nil
}

func (n *syntheticNode) rememberARP(ip net.IP, mac net.HardwareAddr) {
	key := ip.String()
	copied := copyMAC(mac)

	n.arpMu.Lock()
	n.arpCache[key] = copied
	waiters := append([]chan net.HardwareAddr(nil), n.arpWaiters[key]...)
	n.arpMu.Unlock()
	for _, waiter := range waiters {
		select {
		case waiter <- copyMAC(copied):
		default:
		}
	}
}

func (n *syntheticNode) writeFrame(frame []byte) error {
	n.writeMu.Lock()
	defer n.writeMu.Unlock()
	_, err := n.conn.Write(frame)
	return err
}
