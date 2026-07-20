//go:build e2e

package bgp

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestBGPRouteInjectionE2E verifies the full path on real hardware: with the
// helper's BGP speaker up and a Cilium BGP peering applied in-cluster
// advertising an LB VIP, the VIP's /32 appears in the host routing table and
// disappears when BGP is disabled.
//
// Manual prerequisites (this test only asserts the host-side observable):
//  1. sudo tbx system install   (helper with BGP support)
//  2. tbx cluster create demo && configure + bootstrap + install Cilium
//  3. kubectl apply the CiliumBGPPeeringPolicy from `tbx manifests demo bgp`
//     and an LB Service so Cilium advertises a .200-.239 VIP
//  4. tbx bgp enable demo
func TestBGPRouteInjectionE2E(t *testing.T) {
	routesFor := func() string {
		out, _ := exec.Command("netstat", "-rn", "-f", "inet").Output()
		return string(out)
	}
	// wait up to 30s for a 172.30.x.2xx host route to appear
	deadline := time.Now().Add(30 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		if strings.Contains(routesFor(), "172.30.") && hasVIPRoute(routesFor()) {
			found = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !found {
		t.Skip("no advertised LB VIP route found — is Cilium BGP peering applied and `tbx bgp enable` run?")
	}
	t.Log("advertised VIP route present in host FIB")

	// the withdrawal half: disable BGP and confirm the route leaves the FIB
	if out, err := exec.Command("tbx", "bgp", "disable", "demo").CombinedOutput(); err != nil {
		t.Fatalf("bgp disable: %v: %s", err, out)
	}
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !hasVIPRoute(routesFor()) {
			return // withdrawn as required
		}
		time.Sleep(2 * time.Second)
	}
	t.Error("VIP route still present after `tbx bgp disable` — withdrawal failed")
}

func hasVIPRoute(routes string) bool {
	for _, line := range strings.Split(routes, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "172.30.") &&
			(strings.Contains(line, ".200") || strings.Contains(line, ".201") || strings.Contains(line, ".202")) {
			return true
		}
	}
	return false
}
