package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tbxdns "github.com/randax/talos-box/internal/dns"
	"github.com/randax/talos-box/internal/helper"
)

const resolverPath = "/etc/resolver/k8s.test"

func (c cli) runDoctor(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: tbx doctor")
	}
	checks := []struct {
		name string
		run  func() error
	}{
		{name: "helper", run: checkHelper},
		{name: "resolver", run: checkResolver},
		{name: "DNS", run: func() error { return tbxdns.Probe(tbxdns.Address) }},
		{name: "forwarding", run: checkForwarding},
	}
	failed := false
	for _, check := range checks {
		if err := check.run(); err != nil {
			failed = true
			if _, writeErr := fmt.Fprintf(c.out, "FAIL %s: %v\n", check.name, err); writeErr != nil {
				return writeErr
			}
			continue
		}
		if _, err := fmt.Fprintf(c.out, "PASS %s\n", check.name); err != nil {
			return err
		}
	}
	if failed {
		return errors.New("one or more doctor checks failed")
	}
	return nil
}

func checkHelper() error {
	client, err := helper.Connect()
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return client.Ping()
}

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
