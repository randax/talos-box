package helper

import (
	"errors"
	"strings"
	"testing"
)

func TestProtocolMismatchErrorRecommendsSystemInstall(t *testing.T) {
	t.Parallel()

	err := protocolMismatchError(2, 1)
	if !errors.Is(err, errProtocolMismatch) {
		t.Fatalf("protocolMismatchError() = %v, want errProtocolMismatch identity", err)
	}
	message := err.Error()
	if !strings.Contains(message, "(client 2, helper 1)") {
		t.Errorf("message %q missing version facts", message)
	}
	if !strings.Contains(message, "tbx system install") {
		t.Errorf("message %q missing system install remediation", message)
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
	if !strings.Contains(message, "tbx system install") {
		t.Errorf("message %q missing system install remediation", message)
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
	if count := strings.Count(message, "tbx system install"); count != 1 {
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
	if !strings.Contains(message, "tbx system install") {
		t.Errorf("message %q missing system install remediation", message)
	}
}

func TestProtocolHandshakeFailureEmptyDetail(t *testing.T) {
	t.Parallel()

	message := protocolHandshakeFailure("").Error()
	if !strings.Contains(message, "helper rejected the version handshake") {
		t.Errorf("message %q missing default detail", message)
	}
	if !strings.Contains(message, "tbx system install") {
		t.Errorf("message %q missing system install remediation", message)
	}
}
