//go:build linux

package main

import (
	"log"

	"github.com/randax/talos-box/internal/helper"
)

func configureHostNetworking() {
	client, err := helper.Connect()
	if err != nil {
		log.Printf("network helper unavailable: %v", err)
		return
	}
	defer func() { _ = client.Close() }()
	if err := client.EnableForwarding(); err != nil {
		log.Printf("enable IP forwarding: %v", err)
	}
}

// Linux bridge, nftables, and per-interface forwarding state is converged by
// tbx-helper at service start. DNS listeners and resolved registrations have
// their own daemon reconciliation loop.
func startHostNetworkingMaintenance() func() { return func() {} }
