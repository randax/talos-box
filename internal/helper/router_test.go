package helper

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestFrameRouterDHCPRequestImmediatelyEstablishesRoute(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:02")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	var forwarded [][]byte
	targetMAC := mustMAC(t, "02:00:00:00:f1:02")
	targetIP := hostIP(241, 2)
	target := router.addPort(241, func(frame []byte) error {
		forwarded = append(forwarded, append([]byte(nil), frame...))
		return nil
	})
	learnNodeOwner(t, router, target, targetMAC, targetIP)

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		targetIP,
		0x2201,
		1,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if !handled {
		t.Fatal("DHCPREQUEST should immediately establish cross-subnet reachability")
	}
	if len(forwarded) != 1 {
		t.Fatalf("forwarded %d frames, want 1", len(forwarded))
	}
	assertForwardedIPv4Frame(t, forwarded[0], targetMAC, routerGatewayMAC(241), sourceIP, targetIP, 63)
}

func TestFrameRouterLearnsAndMovesVIPViaARP(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:0a")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	vip := hostIP(241, 200)

	var forwardedA [][]byte
	targetAMAC := mustMAC(t, "02:00:00:00:f1:0a")
	targetA := router.addPort(241, func(frame []byte) error {
		forwardedA = append(forwardedA, append([]byte(nil), frame...))
		return nil
	})
	learnVIPOwner(t, router, targetA, targetAMAC, vip)

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		vip,
		0x2202,
		2,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if !handled || len(forwardedA) != 1 {
		t.Fatalf("initial VIP route failed: handled=%v forwardedA=%d", handled, len(forwardedA))
	}

	var forwardedB [][]byte
	targetBMAC := mustMAC(t, "02:00:00:00:f1:0b")
	targetB := router.addPort(241, func(frame []byte) error {
		forwardedB = append(forwardedB, append([]byte(nil), frame...))
		return nil
	})
	learnVIPOwner(t, router, targetB, targetBMAC, vip)

	forwardedA = nil
	forwardedB = nil
	handled, err = router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		vip,
		0x2203,
		3,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if !handled {
		t.Fatal("moved VIP frame was not handled locally")
	}
	if len(forwardedA) != 0 || len(forwardedB) != 1 {
		t.Fatalf("VIP move failed: forwardedA=%d forwardedB=%d", len(forwardedA), len(forwardedB))
	}
}

func TestFrameRouterRejectsSpoofedSourceSubnet(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:21")
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, hostIP(240, 2))

	targetMAC := mustMAC(t, "02:00:00:00:f1:21")
	targetIP := hostIP(241, 2)
	target := router.addPort(241, func([]byte) error { return nil })
	learnNodeOwner(t, router, target, targetMAC, targetIP)

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		hostIP(241, 9),
		targetIP,
		0x2204,
		4,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("spoofed source subnet frame should fall through to vmnet")
	}
}

func TestFrameRouterDoesNotLearnNodeOrVIPFromArbitraryIPv4Source(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:31")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	var forwardedA [][]byte
	targetAMAC := mustMAC(t, "02:00:00:00:f1:31")
	targetA := router.addPort(241, func(frame []byte) error {
		forwardedA = append(forwardedA, append([]byte(nil), frame...))
		return nil
	})
	nodeIP := hostIP(241, 2)
	vip := hostIP(241, 200)
	learnNodeOwner(t, router, targetA, targetAMAC, nodeIP)
	learnVIPOwner(t, router, targetA, targetAMAC, vip)

	targetBMAC := mustMAC(t, "02:00:00:00:f1:32")
	targetB := router.addPort(241, func([]byte) error { return nil })
	assertRouterPassesThrough(t, router, targetB, buildICMPEchoFrameWithTTL(
		targetBMAC,
		mustMAC(t, "02:00:00:00:f1:01"),
		nodeIP,
		hostIP(241, 77),
		0x2205,
		5,
		nil,
		64,
	))
	assertRouterPassesThrough(t, router, targetB, buildICMPEchoFrameWithTTL(
		targetBMAC,
		mustMAC(t, "02:00:00:00:f1:01"),
		vip,
		hostIP(241, 78),
		0x2206,
		6,
		nil,
		64,
	))

	forwardedA = nil
	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		nodeIP,
		0x2207,
		7,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route(node) error = %v", err)
	}
	if !handled || len(forwardedA) != 1 {
		t.Fatalf("node route handled=%v forwardedA=%d, want handled=true forwardedA=1", handled, len(forwardedA))
	}

	forwardedA = nil
	handled, err = router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		vip,
		0x2208,
		8,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route(vip) error = %v", err)
	}
	if !handled || len(forwardedA) != 1 {
		t.Fatalf("vip route handled=%v forwardedA=%d, want handled=true forwardedA=1", handled, len(forwardedA))
	}
}

func TestFrameRouterRejectsUDPLengthBeyondIPTotalLength(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:34")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	targetMAC := mustMAC(t, "02:00:00:00:f1:34")
	targetIP := hostIP(241, 2)
	target := router.addPort(241, func([]byte) error { return nil })
	frame := buildDHCPFrame(targetMAC, 0x22063, dhcpMessageRequest, targetIP, hostIP(241, 1))
	binary.BigEndian.PutUint16(frame[14+20+4:14+20+6], uint16(len(frame)))
	assertRouterPassesThrough(t, router, target, frame)

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		targetIP,
		0x2064,
		64,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("invalid UDP length must not create node ownership")
	}
}

func TestFrameRouterIgnoresDHCPShapedPayloadOnNonUDPPacket(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:33")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	targetMAC := mustMAC(t, "02:00:00:00:f1:33")
	targetIP := hostIP(241, 2)
	target := router.addPort(241, func([]byte) error { return nil })
	assertRouterPassesThrough(t, router, target, buildNonUDPDHCPShapedFrame(targetMAC, targetIP))

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		targetIP,
		0x2062,
		62,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("non-UDP DHCP-shaped payload should not create a node owner")
	}
}

func TestFrameRouterIgnoresDHCPOnlyInEthernetTrailingBytes(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:35")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	targetMAC := mustMAC(t, "02:00:00:00:f1:35")
	targetIP := hostIP(241, 2)
	target := router.addPort(241, func([]byte) error { return nil })
	assertRouterPassesThrough(t, router, target, buildFrameWithTrailingDHCPPayload(targetMAC, targetIP))

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		targetIP,
		0x2066,
		66,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("Ethernet trailing bytes beyond IP totalLen must not create a node owner")
	}
}

func TestFrameRouterRejectsFragmentedDHCPForNodeOwnership(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:36")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	targetMAC := mustMAC(t, "02:00:00:00:f1:36")
	targetIP := hostIP(241, 2)
	target := router.addPort(241, func([]byte) error { return nil })
	frame := buildDHCPFrame(targetMAC, 0x22064, dhcpMessageRequest, targetIP, hostIP(241, 1))
	markIPv4MoreFragments(frame)
	assertRouterPassesThrough(t, router, target, frame)

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		targetIP,
		0x2068,
		68,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("fragmented DHCP must not create a node owner")
	}
}

func TestFrameRouterBadIPv4ChecksumCannotCreateNodeOwnership(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:25")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	targetMAC := mustMAC(t, "02:00:00:00:f1:25")
	targetIP := hostIP(241, 2)
	target := router.addPort(241, func([]byte) error { return nil })

	frame := buildDHCPFrame(targetMAC, 0x22050, dhcpMessageRequest, targetIP, hostIP(241, 1))
	corruptIPv4Checksum(frame)
	assertRouterPassesThrough(t, router, target, frame)

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		targetIP,
		0x2052,
		52,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("bad IPv4 checksum must not create node ownership")
	}
}

func TestFrameRouterLeavesReservedDestinationHostsOnVMNet(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:41")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	for _, dst := range []net.IP{hostIP(241, 1), hostIP(241, 180), hostIP(241, 199), hostIP(241, 240)} {
		handled, err := router.route(source, buildICMPEchoFrameWithTTL(
			sourceMAC,
			mustMAC(t, "02:00:00:00:f0:01"),
			sourceIP,
			dst,
			0x2209,
			uint16(dst[3]),
			nil,
			64,
		))
		if err != nil {
			t.Fatalf("route(%s) error = %v", dst, err)
		}
		if handled {
			t.Fatalf("reserved destination %s should fall through to vmnet", dst)
		}
	}
}

func TestFrameRouterLeavesUnknownTargetOnVMNet(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:51")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	targetMAC := mustMAC(t, "02:00:00:00:f1:51")
	target := router.addPort(241, func([]byte) error { return nil })
	learnNodeOwner(t, router, target, targetMAC, hostIP(241, 2))

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		hostIP(241, 99),
		0x2210,
		10,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("unknown target should not be routed locally")
	}
}

func TestFrameRouterPropagatesTargetSendErrors(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:61")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	targetErr := errors.New("target send failed")
	targetMAC := mustMAC(t, "02:00:00:00:f1:61")
	targetIP := hostIP(241, 2)
	target := router.addPort(241, func([]byte) error { return targetErr })
	learnNodeOwner(t, router, target, targetMAC, targetIP)

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		targetIP,
		0x2211,
		11,
		nil,
		64,
	))
	if !handled {
		t.Fatal("send failure should still report the frame as locally handled")
	}
	if !errors.Is(err, targetErr) {
		t.Fatalf("route() error = %v, want %v", err, targetErr)
	}
}

func TestFrameRouterDoesNotHoldLockDuringForwardSend(t *testing.T) {
	router := newFrameRouter()
	sourceMAC := mustMAC(t, "02:00:00:00:f0:62")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	targetMAC := mustMAC(t, "02:00:00:00:f1:62")
	targetIP := hostIP(241, 2)
	gatewayMAC := mustMAC(t, "02:00:00:00:f0:01")
	sendStarted := make(chan struct{})
	releaseSend := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseSend) })
	}
	defer release()
	target := router.addPort(241, func([]byte) error {
		close(sendStarted)
		<-releaseSend
		return nil
	})
	learnNodeOwner(t, router, target, targetMAC, targetIP)

	type routeResult struct {
		handled bool
		err     error
	}
	routeDone := make(chan routeResult, 1)
	go func() {
		handled, err := router.route(source, buildICMPEchoFrameWithTTL(
			sourceMAC,
			gatewayMAC,
			sourceIP,
			targetIP,
			0x2062,
			62,
			nil,
			64,
		))
		routeDone <- routeResult{handled: handled, err: err}
	}()

	select {
	case <-sendStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("forward send did not start")
	}

	if !router.mu.TryLock() {
		release()
		<-routeDone
		t.Fatal("router lock is held during forwarding send")
	}
	router.mu.Unlock()
	router.addPort(242, func([]byte) error { return nil })
	release()

	result := <-routeDone
	if result.err != nil {
		t.Fatalf("route() error = %v", result.err)
	}
	if !result.handled {
		t.Fatal("route() handled = false, want true")
	}
}

func TestFrameRouterRejectsMismatchedDHCPChaddr(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:71")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	targetMAC := mustMAC(t, "02:00:00:00:f1:71")
	targetIP := hostIP(241, 2)
	target := router.addPort(241, func([]byte) error { return nil })
	frame := buildDHCPFrame(targetMAC, 0x2212, dhcpMessageRequest, targetIP, hostIP(241, 1))
	copy(frame[14+28:14+34], mustMAC(t, "02:00:00:00:f1:72"))
	assertRouterPassesThrough(t, router, target, frame)

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		targetIP,
		0x2213,
		12,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("mismatched DHCP chaddr should not bind a node owner")
	}
}

func TestFrameRouterDoesNotAllowForgedDHCPToStealNode(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:81")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	var forwardedA [][]byte
	targetAMAC := mustMAC(t, "02:00:00:00:f1:81")
	targetAIP := hostIP(241, 2)
	targetA := router.addPort(241, func(frame []byte) error {
		forwardedA = append(forwardedA, append([]byte(nil), frame...))
		return nil
	})
	learnNodeOwner(t, router, targetA, targetAMAC, targetAIP)

	var forwardedB [][]byte
	targetBMAC := mustMAC(t, "02:00:00:00:f1:82")
	targetB := router.addPort(241, func(frame []byte) error {
		forwardedB = append(forwardedB, append([]byte(nil), frame...))
		return nil
	})
	assertRouterPassesThrough(t, router, targetB, buildDHCPFrame(targetBMAC, 0x2214, dhcpMessageRequest, targetAIP, hostIP(241, 1)))

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		targetAIP,
		0x2215,
		13,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if !handled {
		t.Fatal("owned node route was not handled locally")
	}
	if len(forwardedA) != 1 || len(forwardedB) != 0 {
		t.Fatalf("forged DHCP stole ownership: forwardedA=%d forwardedB=%d", len(forwardedA), len(forwardedB))
	}
}

func TestFrameRouterSamePortNodeRebindReplacesOldOwnership(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:91")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	var forwarded [][]byte
	targetMAC := mustMAC(t, "02:00:00:00:f1:91")
	target := router.addPort(241, func(frame []byte) error {
		forwarded = append(forwarded, append([]byte(nil), frame...))
		return nil
	})
	oldIP := hostIP(241, 2)
	newIP := hostIP(241, 3)
	learnNodeOwner(t, router, target, targetMAC, oldIP)
	assertRouterPassesThrough(t, router, target, buildDHCPFrame(targetMAC, 0x22910, dhcpMessageRequest, newIP, hostIP(241, 1)))

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		oldIP,
		0x2216,
		14,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route(old) error = %v", err)
	}
	if handled {
		t.Fatal("same-port rebind should remove old node ownership")
	}

	forwarded = nil
	handled, err = router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		newIP,
		0x2217,
		15,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route(new) error = %v", err)
	}
	if !handled || len(forwarded) != 1 {
		t.Fatalf("same-port rebind did not establish new node ownership: handled=%v forwarded=%d", handled, len(forwarded))
	}
}

func TestFrameRouterSamePortRebindCannotBeStolenByOtherPort(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:a1")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	var forwardedA [][]byte
	targetAMAC := mustMAC(t, "02:00:00:00:f1:a1")
	targetA := router.addPort(241, func(frame []byte) error {
		forwardedA = append(forwardedA, append([]byte(nil), frame...))
		return nil
	})
	learnNodeOwner(t, router, targetA, targetAMAC, hostIP(241, 2))
	assertRouterPassesThrough(t, router, targetA, buildDHCPFrame(targetAMAC, 0x22911, dhcpMessageRequest, hostIP(241, 3), hostIP(241, 1)))

	var forwardedB [][]byte
	targetBMAC := mustMAC(t, "02:00:00:00:f1:a2")
	targetB := router.addPort(241, func(frame []byte) error {
		forwardedB = append(forwardedB, append([]byte(nil), frame...))
		return nil
	})
	assertRouterPassesThrough(t, router, targetB, buildDHCPFrame(targetBMAC, 0x22912, dhcpMessageRequest, hostIP(241, 3), hostIP(241, 1)))

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		hostIP(241, 3),
		0x2218,
		16,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if !handled {
		t.Fatal("same-port rebound node should stay reachable")
	}
	if len(forwardedA) != 1 || len(forwardedB) != 0 {
		t.Fatalf("rebound node ownership was stolen: forwardedA=%d forwardedB=%d", len(forwardedA), len(forwardedB))
	}
}

func TestFrameRouterDoesNotBindNodeOwnerFromARP(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:b1")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	targetMAC := mustMAC(t, "02:00:00:00:f1:b1")
	target := router.addPort(241, func([]byte) error { return nil })
	assertRouterPassesThrough(t, router, target, buildRouterTestARPReply(hostIP(241, 2), targetMAC, mustMAC(t, "ff:ff:ff:ff:ff:ff"), sourceIP))

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		hostIP(241, 2),
		0x2219,
		17,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("ARP sender evidence should not bind node ownership")
	}
}

func TestFrameRouterRejectsMismatchedSourceMAC(t *testing.T) {
	router := newFrameRouter()

	targetMAC := mustMAC(t, "02:00:00:00:f1:c1")
	targetIP := hostIP(241, 2)
	target := router.addPort(241, func([]byte) error { return nil })
	learnNodeOwner(t, router, target, targetMAC, targetIP)

	handled, err := router.route(target, buildICMPEchoFrameWithTTL(
		mustMAC(t, "02:00:00:00:f1:c2"),
		mustMAC(t, "02:00:00:00:f1:01"),
		targetIP,
		hostIP(240, 2),
		0x2220,
		18,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("mismatched source MAC should not be eligible for local routing")
	}
}

func TestFrameRouterLeavesSameSubnetTrafficOnVMNet(t *testing.T) {
	router := newFrameRouter()

	sourceMAC := mustMAC(t, "02:00:00:00:f0:d1")
	sourceIP := hostIP(240, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	learnNodeOwner(t, router, source, sourceMAC, sourceIP)

	handled, err := router.route(source, buildICMPEchoFrameWithTTL(
		sourceMAC,
		mustMAC(t, "02:00:00:00:f0:01"),
		sourceIP,
		hostIP(240, 3),
		0x2221,
		19,
		nil,
		64,
	))
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("same-subnet frame should fall through to vmnet")
	}
}

func BenchmarkFrameRouterRoute(b *testing.B) {
	router := newFrameRouter()
	sourceMAC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0xf0, 0xee}
	targetMAC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0xf1, 0xee}
	sourceIP := hostIP(240, 2)
	targetIP := hostIP(241, 2)
	source := router.addPort(240, func([]byte) error { return nil })
	target := router.addPort(241, func([]byte) error { return nil })

	if handled, err := router.route(source, buildDHCPFrame(sourceMAC, 1, dhcpMessageRequest, sourceIP, hostIP(240, 1))); err != nil || handled {
		b.Fatalf("learn source: handled=%t error=%v", handled, err)
	}
	if handled, err := router.route(target, buildDHCPFrame(targetMAC, 2, dhcpMessageRequest, targetIP, hostIP(241, 1))); err != nil || handled {
		b.Fatalf("learn target: handled=%t error=%v", handled, err)
	}
	frame := buildICMPEchoFrameWithTTL(
		sourceMAC,
		net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0xf0, 0x01},
		sourceIP,
		targetIP,
		0x20ee,
		238,
		nil,
		64,
	)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		handled, err := router.route(source, frame)
		if err != nil || !handled {
			b.Fatalf("route: handled=%t error=%v", handled, err)
		}
	}
}

func learnNodeOwner(t *testing.T, router *frameRouter, port *routerPort, mac net.HardwareAddr, ip net.IP) {
	t.Helper()
	assertRouterPassesThrough(t, router, port, buildDHCPFrame(mac, 0x1234, dhcpMessageRequest, ip, hostIP(port.subnet, 1)))
}

func learnVIPOwner(t *testing.T, router *frameRouter, port *routerPort, mac net.HardwareAddr, ip net.IP) {
	t.Helper()
	assertRouterPassesThrough(t, router, port, buildGratuitousARP(mac, ip))
}

func assertRouterPassesThrough(t *testing.T, router *frameRouter, port *routerPort, frame []byte) {
	t.Helper()
	handled, err := router.route(port, frame)
	if err != nil {
		t.Fatalf("route() error = %v", err)
	}
	if handled {
		t.Fatal("frame should have fallen through to vmnet")
	}
}

func assertForwardedIPv4Frame(
	t *testing.T,
	frame []byte,
	wantDstMAC, wantSrcMAC net.HardwareAddr,
	wantSrcIP, wantDstIP net.IP,
	wantTTL byte,
) {
	t.Helper()

	dstMAC, srcMAC, etherType, payload, ok := parseEthernetFrame(frame)
	if !ok {
		t.Fatal("forwarded frame is not valid Ethernet")
	}
	if etherType != etherTypeIPv4 {
		t.Fatalf("etherType = %#04x, want IPv4", etherType)
	}
	if !macEqual(dstMAC, wantDstMAC) {
		t.Fatalf("dst MAC = %s, want %s", dstMAC, wantDstMAC)
	}
	if !macEqual(srcMAC, wantSrcMAC) {
		t.Fatalf("src MAC = %s, want %s", srcMAC, wantSrcMAC)
	}

	header, _, ok := parseIPv4Header(payload)
	if !ok {
		t.Fatal("forwarded frame is not valid IPv4")
	}
	srcIP := net.IP(header[12:16]).To4()
	dstIP := net.IP(header[16:20]).To4()
	if !srcIP.Equal(wantSrcIP) {
		t.Fatalf("src IP = %s, want %s", srcIP, wantSrcIP)
	}
	if !dstIP.Equal(wantDstIP) {
		t.Fatalf("dst IP = %s, want %s", dstIP, wantDstIP)
	}
	if header[8] != wantTTL {
		t.Fatalf("ttl = %d, want %d", header[8], wantTTL)
	}
	if checksum(header) != 0 {
		t.Fatalf("IPv4 header checksum invalid: %#04x", binary.BigEndian.Uint16(header[10:12]))
	}
}

func buildRouterTestARPReply(senderIP net.IP, senderMAC, targetMAC net.HardwareAddr, targetIP net.IP) []byte {
	payload := make([]byte, 28)
	binary.BigEndian.PutUint16(payload[0:2], 1)
	binary.BigEndian.PutUint16(payload[2:4], etherTypeIPv4)
	payload[4] = 6
	payload[5] = 4
	binary.BigEndian.PutUint16(payload[6:8], routerARPOpReply)
	copy(payload[8:14], senderMAC)
	copy(payload[14:18], senderIP.To4())
	copy(payload[18:24], targetMAC)
	copy(payload[24:28], targetIP.To4())
	return buildEthernetFrame(targetMAC, senderMAC, etherTypeARP, payload)
}

func buildNonUDPDHCPShapedFrame(srcMAC net.HardwareAddr, requestedIP net.IP) []byte {
	dhcpFrame := buildDHCPFrame(srcMAC, 0x7777, dhcpMessageRequest, requestedIP, hostIP(241, 1))
	udpPayload := append([]byte(nil), dhcpFrame[14+20+8:]...)
	return buildIPv4EthernetFrame(
		net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0xff, 0x02},
		srcMAC,
		requestedIP,
		hostIP(242, 3),
		ipProtocolICMP,
		udpPayload,
	)
}

func buildFrameWithTrailingDHCPPayload(srcMAC net.HardwareAddr, srcIP net.IP) []byte {
	frame := buildICMPEchoFrameWithTTL(
		srcMAC,
		net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0xff, 0x03},
		srcIP,
		hostIP(242, 4),
		0x2069,
		69,
		nil,
		64,
	)
	return append(frame, buildDHCPFrame(srcMAC, 0x8888, dhcpMessageRequest, srcIP, hostIP(241, 1))[14:]...)
}

func corruptIPv4Checksum(frame []byte) {
	frame[14+10] ^= 0xff
}

func markIPv4MoreFragments(frame []byte) {
	flagsOffset := binary.BigEndian.Uint16(frame[14+6 : 14+8])
	flagsOffset |= 0x2000
	binary.BigEndian.PutUint16(frame[14+6:14+8], flagsOffset)
	header := append([]byte(nil), frame[14:14+20]...)
	header[10] = 0
	header[11] = 0
	binary.BigEndian.PutUint16(frame[14+10:14+12], checksum(header))
}
