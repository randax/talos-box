package daemon

import (
	"os"
	"testing"
)

// TestMain pins the host readings for the whole package. Any test that reaches
// checkProvisionStart without stubbing them would otherwise be measuring the
// runner it happens to be on: a busy laptop or a small CI box has less free
// memory than the balloon reserve, and the start gate then refuses a create the
// test was not written to be about. Generous, deterministic values keep the
// gate open by default; a test that is *about* the gate sets the Server's own
// hostFreeMemory/hostTotalMemory/hostPressure, which always win over these.
func TestMain(m *testing.M) {
	measureHostFreeMiB = plentifulHostMemory
	measureHostTotalMiB = plentifulHostMemory
	measureHostPressure = noHostPressure
	// The BGP port inventory shells out to the host's netstat, whose listeners
	// are the runner's business and not any test's: an unrelated process on
	// port 179 must not add an advisory to a `bgp enable` a test is asserting
	// on. Tests about the inventory stub it themselves.
	bgpPortListeners = nil
	os.Exit(m.Run())
}
