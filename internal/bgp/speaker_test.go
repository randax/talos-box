package bgp

import (
	"testing"
	"time"
)

// TestSpeakerStartsAndStops verifies the GoBGP integration initializes on this
// host (StartBgp + peer group + dynamic neighbor + RIB watch) without root, on
// a high port. It does not exercise a live peer — that is the e2e AC.
func TestSpeakerStartsAndStops(t *testing.T) {
	fib := newFakeFIB()
	speaker, err := startSpeakerOnPort(64512, 64600, "127.0.0.1", "127.0.0.0/24", fib, 17900)
	if err != nil {
		t.Fatalf("speaker failed to start: %v", err)
	}
	// give the watch goroutine a beat to attach; no peers, so no routes
	time.Sleep(200 * time.Millisecond)
	if len(fib.routes) != 0 {
		t.Errorf("routes injected with no peer: %v", fib.routes)
	}
	speaker.Stop()
	// a second start on the same port must succeed (port released on Stop)
	speaker2, err := startSpeakerOnPort(64512, 64600, "127.0.0.1", "127.0.0.0/24", fib, 17900)
	if err != nil {
		t.Fatalf("restart failed (port not released?): %v", err)
	}
	speaker2.Stop()
}
