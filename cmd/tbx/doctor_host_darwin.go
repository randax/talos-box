//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tbxdns "github.com/randax/talos-box/internal/dns"
)

const resolverPath = "/etc/resolver/k8s.test"

func checkResolver() error {
	info, err := os.Stat(resolverPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("resolver path is not a regular file")
	}
	return nil
}

func checkPlatformDirectDNS() error { return tbxdns.Probe(tbxdns.Address) }

func checkForwarding() error {
	output, err := exec.Command("/usr/sbin/sysctl", "-n", "net.inet.ip.forwarding").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) != "1" {
		return fmt.Errorf("net.inet.ip.forwarding is %q, want 1", strings.TrimSpace(string(output)))
	}
	return nil
}

func classifyResolverFailure(err error) (string, string) { return "FAIL", err.Error() }

func classifySystemDNSFailure(err error) (string, string) { return "FAIL", err.Error() }
