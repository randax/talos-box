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
