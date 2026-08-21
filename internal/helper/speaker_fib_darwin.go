//go:build darwin && cgo

package helper

import "github.com/randax/talos-box/internal/bgp"

// speakerFIB is what a cluster's BGP speaker writes learned VIPs into. The
// vmnet build routes guest-to-guest traffic in userspace, so the frame router
// has to see the same routes the kernel does.
func speakerFIB() bgp.FIB {
	return newRoutedVIPFIB(routeFIB{}, helperFrameRouter)
}
