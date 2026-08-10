//go:build !linux

package helper

// SocketPath returns the system helper socket used on non-Linux platforms.
func SocketPath() (string, error) { return systemHelperSocketPath, nil }

// ServerSocketPath returns the system helper socket used on non-Linux platforms.
func ServerSocketPath(*uint32) (string, error) { return systemHelperSocketPath, nil }
