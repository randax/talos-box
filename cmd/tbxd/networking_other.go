//go:build !darwin && !linux

package main

func configureHostNetworking() {}

func startHostNetworkingMaintenance() func() { return func() {} }
