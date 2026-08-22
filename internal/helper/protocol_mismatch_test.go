package helper

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestProtocolMismatchErrorRecommendsAHelperReinstall(t *testing.T) {
	t.Parallel()

	err := protocolMismatchError(2, 1)
	if !errors.Is(err, errProtocolMismatch) {
		t.Fatalf("protocolMismatchError() = %v, want errProtocolMismatch identity", err)
	}
	message := err.Error()
	if !strings.Contains(message, "(client 2, helper 1)") {
		t.Errorf("message %q missing version facts", message)
	}
	if !strings.Contains(message, protocolMismatchAdvice()) {
		t.Errorf("message %q missing the host's reinstall remediation", message)
	}
	if strings.Contains(message, "restart the helper") {
		t.Errorf("message %q still recommends restarting the helper", message)
	}
}

func TestProtocolHandshakeFailureDoesNotDoubleWrapMismatch(t *testing.T) {
	t.Parallel()

	// An old helper rejects the handshake with its own (stale) mismatch message;
	// the client must not nest it or keep the stale advice.
	err := protocolHandshakeFailure("helper protocol mismatch (client 2, helper 1): restart the helper")
	if !errors.Is(err, errProtocolMismatch) {
		t.Fatalf("protocolHandshakeFailure() = %v, want errProtocolMismatch identity", err)
	}
	message := err.Error()
	if count := strings.Count(message, "helper protocol mismatch"); count != 1 {
		t.Errorf("message %q repeats the mismatch prefix %d times, want 1", message, count)
	}
	if !strings.Contains(message, "(client 2, helper 1)") {
		t.Errorf("message %q missing version facts", message)
	}
	if !strings.Contains(message, protocolMismatchAdvice()) {
		t.Errorf("message %q missing the host's reinstall remediation", message)
	}
	if strings.Contains(message, "restart the helper") {
		t.Errorf("message %q still recommends restarting the helper", message)
	}
}

func TestProtocolHandshakeFailureDoesNotDoubleWrapCurrentMismatch(t *testing.T) {
	t.Parallel()

	// A current helper rejects with the new wording; still exactly one wrap.
	err := protocolHandshakeFailure(protocolMismatchError(2, 3).Error())
	message := err.Error()
	if count := strings.Count(message, "helper protocol mismatch"); count != 1 {
		t.Errorf("message %q repeats the mismatch prefix %d times, want 1", message, count)
	}
	if count := strings.Count(message, protocolMismatchAdvice()); count != 1 {
		t.Errorf("message %q repeats the remediation %d times, want 1", message, count)
	}
	if !strings.Contains(message, "(client 2, helper 3)") {
		t.Errorf("message %q missing version facts", message)
	}
}

func TestProtocolHandshakeFailureWrapsForeignDetailOnce(t *testing.T) {
	t.Parallel()

	err := protocolHandshakeFailure(`unknown operation "helper.info"`)
	if !errors.Is(err, errProtocolMismatch) {
		t.Fatalf("protocolHandshakeFailure() = %v, want errProtocolMismatch identity", err)
	}
	message := err.Error()
	if !strings.Contains(message, `unknown operation "helper.info"`) {
		t.Errorf("message %q missing helper detail", message)
	}
	if !strings.Contains(message, protocolMismatchAdvice()) {
		t.Errorf("message %q missing the host's reinstall remediation", message)
	}
}

func TestProtocolMismatchAdviceQuotesPathsWithSpaces(t *testing.T) {
	t.Parallel()

	advice := protocolMismatchAdviceFor("/Users/o r/projects/talos box/bin/tbxd", nil)
	if !strings.Contains(advice, "sudo '/Users/o r/projects/talos box/bin/tbx' system install") {
		t.Errorf("advice %q does not shell-quote the tbx path", advice)
	}

	plain := protocolMismatchAdviceFor("/opt/talosbox/bin/tbxd", nil)
	if !strings.Contains(plain, "sudo /opt/talosbox/bin/tbx system install") {
		t.Errorf("advice %q needlessly quotes a safe path", plain)
	}

	fallback := protocolMismatchAdviceFor("", errors.New("unknown executable"))
	if !strings.Contains(fallback, "tbx system install") {
		t.Errorf("fallback advice %q missing remediation", fallback)
	}
}

// `tbx system install` installs the macOS launchd helper, which docs/linux.md
// forbids on Linux: a Linux upgrader hitting the protocol bump has to be sent
// to the package/systemd path instead (#448 follow-up).
func TestProtocolMismatchAdviceFollowsTheHostInstallMechanism(t *testing.T) {
	t.Parallel()

	linux := protocolMismatchAdviceForGOOS("linux", "/usr/bin/tbxd", nil)
	if strings.Contains(linux, "system install") {
		t.Errorf("Linux advice %q sends the operator to `tbx system install`", linux)
	}
	if !strings.Contains(linux, "tbx-helper") || !strings.Contains(linux, "systemctl restart tbx-helper.socket") {
		t.Errorf("Linux advice %q missing the package upgrade and socket restart", linux)
	}

	darwin := protocolMismatchAdviceForGOOS("darwin", "/opt/talosbox/bin/tbxd", nil)
	if !strings.Contains(darwin, "sudo /opt/talosbox/bin/tbx system install") {
		t.Errorf("macOS advice %q changed", darwin)
	}

	if runtime.GOOS == "linux" && protocolMismatchAdvice() != linux {
		t.Errorf("advice on this host = %q, want the Linux advice", protocolMismatchAdvice())
	}
}

func TestProtocolHandshakeFailureEmptyDetail(t *testing.T) {
	t.Parallel()

	message := protocolHandshakeFailure("").Error()
	if !strings.Contains(message, "helper rejected the version handshake") {
		t.Errorf("message %q missing default detail", message)
	}
	if !strings.Contains(message, protocolMismatchAdvice()) {
		t.Errorf("message %q missing the host's reinstall remediation", message)
	}
}
