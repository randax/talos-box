//go:build e2e

package balloon

import "testing"

// TestBalloonInflatesUnderPressure documents the live e2e verified on this
// machine (#38): a configured node with virtio_balloon loaded, run under a
// synthetic deficit (TBX_BALLOON_RESERVE_MIB raised above host free), had its
// balloon target driven to configured-minus-deficit and its guest MemFree
// collapse from ~2.45 GiB (deflated) to ~600 MiB (inflated) — ~1.8 GiB
// reclaimed, matching the deficit. Deflation (deficit=0) returned it to 2.45 GiB.
//
// Repeat the manual cycle with the #498 policy boundaries: a deficit below
// 256 MiB must not move a target; a deficit at or above 256 MiB may move it
// once, but oscillation may not produce another successful aggregate retarget
// for 60 seconds. With swap/compressor pressure latched, crossing back above
// the reserve must retain existing reclaim without creating new reclaim from
// pressure alone. A one-MiB admission hold must still apply immediately.
//
// The procedure requires a running tbxd + helper + a configured node, so it is
// driven manually; this placeholder records the validated result and keeps the
// e2e tag present for the package.
func TestBalloonInflatesUnderPressure(t *testing.T) {
	t.Skip("manual e2e: see the #38 resolution for the verified inflate/deflate cycle")
}
