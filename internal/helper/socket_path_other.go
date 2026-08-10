//go:build !linux

package helper

import "os"

// SocketPath returns the system helper socket used on non-Linux platforms.
func SocketPath() (string, error) {
	if path, err := socketPathOverride(os.Getenv(helperSocketEnv)); path != "" || err != nil {
		return path, err
	}
	return systemHelperSocketPath, nil
}

// ServerSocketPath returns the system helper socket used on non-Linux platforms.
func ServerSocketPath(*uint32) (string, error) {
	if path, err := socketPathOverride(os.Getenv(helperSocketEnv)); path != "" || err != nil {
		return path, err
	}
	return systemHelperSocketPath, nil
}
