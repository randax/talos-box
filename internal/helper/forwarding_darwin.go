//go:build darwin

package helper

import (
	"fmt"
	"os/exec"
	"strings"
)

func enableForwarding() error {
	output, err := exec.Command("/usr/sbin/sysctl", "-w", "net.inet.ip.forwarding=1").CombinedOutput()
	if err != nil {
		return fmt.Errorf("enable IP forwarding: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func convergeNetworking([]int) error { return nil }

// TeardownSubnet has nothing to remove on macOS: vmnet owns the shared bridge
// and reclaims it when the last interface on it goes away, so a destroyed
// cluster leaves no per-subnet host link behind.
func TeardownSubnet(int) (bool, error) { return false, nil }
