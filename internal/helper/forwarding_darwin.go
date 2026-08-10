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

func convergeNetworking() error { return nil }
