package helper

// ProtocolMismatchErrorForTest builds the handshake refusal for tests in other
// packages, which cannot see the unexported constructor.
func ProtocolMismatchErrorForTest(clientVersion, helperVersion int) error {
	return protocolMismatchError(clientVersion, helperVersion)
}
