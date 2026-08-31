//go:build !darwin && !linux

package main

import "github.com/randax/talos-box/internal/daemon"

func configureHostNetworking() {}

func startHostNetworkingMaintenance(_ *daemon.Server) func() { return func() {} }
