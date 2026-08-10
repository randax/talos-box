//go:build !linux

package helper

import (
	"fmt"
	"os"
)

func platformBindDNS(subnetIndex int) (*os.File, error) {
	return nil, fmt.Errorf("binding DNS for subnet %d is unsupported on this platform", subnetIndex)
}

func platformRegisterDNS(clusterName string, subnetIndex int) DNSRegistration {
	return DNSRegistration{Detail: "systemd-resolved is unavailable on this platform"}
}

func platformUnregisterDNS(int) error { return nil }
