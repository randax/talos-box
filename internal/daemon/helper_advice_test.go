package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/helper"
)

// The helper-unavailable error is the most-read remediation in the daemon log,
// and on Linux it used to print `sudo tbx system install` — the macOS launchd
// install docs/linux.md forbids. It now carries whatever this host's advice is
// (#468).
func TestHelperInstallErrorCarriesTheHostsAdvice(t *testing.T) {
	t.Parallel()

	cause := errors.New("dial unix: connection refused")
	err := helperInstallError(cause)
	if !errors.Is(err, cause) {
		t.Fatalf("helperInstallError() = %v, want the cause wrapped", err)
	}
	if !strings.Contains(err.Error(), helper.UnavailableAdvice()) {
		t.Errorf("message %q missing the host's advice %q", err, helper.UnavailableAdvice())
	}
}

// A stale helper is refused at the handshake with its own advice (upgrade the
// helper). Prefixing that with "enable the socket" would put the wrong
// remediation first, on the one release where every upgrade hits it.
func TestHelperInstallErrorLeavesAProtocolMismatchAlone(t *testing.T) {
	t.Parallel()

	cause := helper.ProtocolMismatchErrorForTest(5, 4)
	err := helperInstallError(cause)
	if err != cause {
		t.Fatalf("helperInstallError() = %v, want the mismatch returned unchanged", err)
	}
	if strings.Contains(err.Error(), helper.UnavailableAdvice()) {
		t.Errorf("message %q stacks the unavailable advice on a mismatch", err)
	}
}
