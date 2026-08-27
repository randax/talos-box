//go:build !linux

package helper

import "github.com/randax/talos-box/internal/cluster"

type noopDHCPManager struct{}

func newPlatformDHCPManager(func() []cluster.Cluster, func() []int) dhcpManager {
	return noopDHCPManager{}
}

func (noopDHCPManager) Converge() error { return nil }

func (noopDHCPManager) Release(int) error { return nil }

func (noopDHCPManager) Close() error { return nil }
