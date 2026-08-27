//go:build !darwin

package main

// securityInventoryFindings takes the macOS system-extension inventory. No
// other platform has one — `systemextensionsctl` is a macOS binary — so this
// reports nothing rather than an INFO about a tool that was never going to be
// there (#468).
func securityInventoryFindings(_ commandOutput) []doctorFinding {
	return nil
}
