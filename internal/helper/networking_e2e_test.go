//go:build e2e

package helper

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	helperE2ESubnetA = 240
	helperE2ESubnetB = 241

	frameReadSize       = 2048
	networkProbeTimeout = 12 * time.Second
	requestRetryDelay   = 1 * time.Second
	hostPingTimeout     = 8 * time.Second
)

type helperNetworkingEnv struct {
	client   *Client
	pingPath string
	runID    string
}

type syntheticNodeHarness struct {
	t   *testing.T
	env helperNetworkingEnv
}

// TestHelperNetworkingE2E exercises the mandatory physical-Mac helper path for
// issue #22: two real helper attachments on distinct subnets, DHCP on both
// links, inter-subnet forwarding, and host reachability to each subnet's .200
// VIP via synthetic endpoints speaking raw Ethernet.
func TestHelperNetworkingE2E(t *testing.T) {
	env := requireHelperNetworking(t)
	harness := newSyntheticNodeHarness(t, env)
	harness.Run()
}

func requireHelperNetworking(t *testing.T) helperNetworkingEnv {
	t.Helper()

	if runtime.GOOS != "darwin" {
		t.Skip("helper networking e2e requires macOS")
	}
	requiredPID, err := requiredHelperE2EPID()
	if err != nil {
		t.Skipf("%v", err)
	}
	activePID, err := activeHelperPID()
	if err != nil {
		t.Fatalf("inspect active %s pid: %v", helperLaunchdLabel, err)
	}
	if activePID != requiredPID {
		t.Fatalf("%s=%d but active %s pid is %d; rebuild/restart helper and export the current pid before running e2e", helperE2EPIDEnv, requiredPID, helperLaunchdLabel, activePID)
	}
	if _, err := os.Stat(SocketPath); err != nil {
		t.Skipf("tbx-helper socket unavailable at %s: %v", SocketPath, err)
	}

	pingPath := "/sbin/ping"
	if _, err := os.Stat(pingPath); err != nil {
		if resolved, lookErr := exec.LookPath("ping"); lookErr == nil {
			pingPath = resolved
		} else {
			t.Skipf("ping binary unavailable: %v", err)
		}
	}

	client, err := Connect()
	if err != nil {
		t.Skipf("tbx-helper not reachable (run `sudo tbx system install`?): %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	if err := client.Ping(); err != nil {
		t.Skipf("tbx-helper not usable for this user/platform: %v", err)
	}

	return helperNetworkingEnv{
		client:   client,
		pingPath: pingPath,
		runID:    fmt.Sprintf("pid%d", os.Getpid()),
	}
}

func newSyntheticNodeHarness(t *testing.T, env helperNetworkingEnv) *syntheticNodeHarness {
	t.Helper()
	return &syntheticNodeHarness{t: t, env: env}
}

func (h *syntheticNodeHarness) Run() {
	h.t.Helper()

	if err := h.env.client.EnableForwarding(); err != nil {
		h.t.Fatalf("enable host forwarding: %v", err)
	}

	nodeA := h.attachNode("a", helperE2ESubnetA, mustMAC(h.t, "02:00:22:f0:00:0a"))
	nodeB := h.attachNode("b", helperE2ESubnetB, mustMAC(h.t, "02:00:22:f1:00:0b"))

	leaseA := nodeA.acquireDHCPLease()
	leaseB := nodeB.acquireDHCPLease()
	h.t.Logf("node A lease=%s subnet=%d", leaseA, nodeA.subnet)
	h.t.Logf("node B lease=%s subnet=%d", leaseB, nodeB.subnet)

	nodeA.pingIP(nodeB.ip)
	nodeB.pingIP(nodeA.ip)

	h.requireHostPing(nodeA.ip)
	h.requireHostPing(nodeB.ip)
	nodeA.pingIP(nodeA.gateway)
	nodeB.pingIP(nodeB.gateway)

	nodeA.claimVIP(hostIP(nodeA.subnet, 200))
	nodeB.claimVIP(hostIP(nodeB.subnet, 200))

	h.requireHostPing(nodeA.vip)
	h.requireHostPing(nodeB.vip)

	nodeA.pingIP(nodeB.vip)
	nodeB.pingIP(nodeA.vip)
}

func (h *syntheticNodeHarness) attachNode(label string, subnet int, mac net.HardwareAddr) *syntheticNode {
	h.t.Helper()

	cluster := fmt.Sprintf("issue22-%s-%s", label, h.env.runID)
	name := fmt.Sprintf("node-%s", label)
	fd, err := h.env.client.Attach(cluster, subnet, name)
	if err != nil {
		h.t.Fatalf("attach %s subnet %d: %v", label, subnet, err)
	}
	if socketType, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE); err != nil {
		h.t.Fatalf("inspect helper fd for %s: %v", label, err)
	} else if socketType != unix.SOCK_DGRAM {
		h.t.Fatalf("helper fd for %s has type %d, want SOCK_DGRAM", label, socketType)
	}

	file := os.NewFile(uintptr(fd), fmt.Sprintf("helper-%s", label))
	connAny, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		h.t.Fatalf("convert helper fd for %s to conn: %v", label, err)
	}
	conn, ok := connAny.(*net.UnixConn)
	if !ok {
		_ = connAny.Close()
		h.t.Fatalf("helper fd for %s is %T, want *net.UnixConn", label, connAny)
	}

	node := newSyntheticNode(h.t, cluster, name, subnet, mac, conn)
	go node.readLoop()

	h.t.Cleanup(func() {
		if err := h.env.client.Detach(cluster, name); err != nil {
			h.t.Errorf("detach %s/%s: %v", cluster, name, err)
		}
		node.close()
	})
	return node
}

func (h *syntheticNodeHarness) requireHostPing(ip net.IP) {
	h.t.Helper()

	_ = exec.Command("/usr/sbin/arp", "-d", ip.String()).Run()
	ctx, cancel := context.WithTimeout(context.Background(), hostPingTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, h.env.pingPath, "-n", "-c", "1", "-W", "2000", ip.String())
	out, err := cmd.CombinedOutput()
	if err == nil {
		h.t.Logf("host ping %s succeeded", ip)
		return
	}

	for attempt := 2; attempt <= 3; attempt++ {
		time.Sleep(requestRetryDelay)
		_ = exec.Command("/usr/sbin/arp", "-d", ip.String()).Run()
		ctx, cancel := context.WithTimeout(context.Background(), hostPingTimeout)
		cmd = exec.CommandContext(ctx, h.env.pingPath, "-n", "-c", "1", "-W", "2000", ip.String())
		out, err = cmd.CombinedOutput()
		cancel()
		if err == nil {
			h.t.Logf("host ping %s succeeded on attempt %d", ip, attempt)
			return
		}
	}

	h.t.Fatalf("host ping %s failed: %v: %s", ip, err, strings.TrimSpace(string(out)))
}
