//go:build darwin

package helper

import (
	"fmt"
	"os/exec"
	"strings"
)

// routeFIB injects host routes into the macOS routing table via /sbin/route.
// Runs in the root helper, so exec of a privileged route command is fine.
type routeFIB struct{}

func (routeFIB) AddHostRoute(prefix, nexthop string) error {
	// route add -host <ip> <gateway>; -host expects a bare address.
	// argv is passed directly (no shell), so peer-controlled prefix/nexthop
	// can't inject. The "File exists"/"not in table" string matching below is
	// coupled to /sbin/route's messages — acceptable for a fixed platform tool.
	ip := strings.TrimSuffix(prefix, "/32")
	out, err := exec.Command("/sbin/route", "-n", "add", "-host", ip, nexthop).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "File exists") {
		return fmt.Errorf("route add %s -> %s: %w: %s", ip, nexthop, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (routeFIB) DeleteHostRoute(prefix string) error {
	ip := strings.TrimSuffix(prefix, "/32")
	out, err := exec.Command("/sbin/route", "-n", "delete", "-host", ip).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "not in table") {
		return fmt.Errorf("route delete %s: %w: %s", ip, err, strings.TrimSpace(string(out)))
	}
	return nil
}
