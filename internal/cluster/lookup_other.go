//go:build !darwin && !linux

package cluster

// LookupIP is unavailable on unsupported platforms.
func LookupIP(string, int) string { return "" }
