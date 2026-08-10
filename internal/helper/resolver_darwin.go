//go:build darwin

package helper

import (
	"errors"
	"fmt"
	"os"
)

func installHostResolver(port int) error { return installResolver(resolverPath, port) }

func uninstallHostResolver() error {
	err := os.Remove(resolverPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove resolver file: %w", err)
	}
	return nil
}
