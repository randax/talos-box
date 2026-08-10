//go:build !linux

package helper

type noopDHCPManager struct{}

func newPlatformDHCPManager() dhcpManager { return noopDHCPManager{} }

func (noopDHCPManager) Converge() error { return nil }

func (noopDHCPManager) Close() error { return nil }
