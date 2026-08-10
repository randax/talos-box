//go:build !darwin && !linux

package main

import (
	"errors"

	tbxdns "github.com/randax/talos-box/internal/dns"
)

func checkResolver() error { return errors.New("host resolver integration is unsupported") }

func checkPlatformDirectDNS() error { return tbxdns.Probe(tbxdns.Address) }

func checkForwarding() error { return errors.New("IP forwarding check is unsupported") }

func classifyResolverFailure(err error) (string, string) { return "FAIL", err.Error() }

func classifySystemDNSFailure(err error) (string, string) { return "FAIL", err.Error() }
