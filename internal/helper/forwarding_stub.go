//go:build !darwin && !linux

package helper

import "errors"

func enableForwarding() error {
	return errors.New("IP forwarding is unavailable on this platform")
}

func convergeNetworking([]int) error { return nil }

func TeardownSubnet(int) (bool, error) { return false, nil }
