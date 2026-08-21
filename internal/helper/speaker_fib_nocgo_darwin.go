//go:build darwin && !cgo

package helper

import "github.com/randax/talos-box/internal/bgp"

// speakerFIB without cgo has no vmnet interfaces and therefore no frame
// router: the kernel routing table is the whole story.
func speakerFIB() bgp.FIB {
	return routeFIB{}
}
