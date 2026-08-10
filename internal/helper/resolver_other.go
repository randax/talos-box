//go:build !darwin

package helper

import "errors"

func installHostResolver(int) error {
	return errors.New("resolver-file installation is only supported on macOS")
}

func uninstallHostResolver() error { return nil }
