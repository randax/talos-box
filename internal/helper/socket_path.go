package helper

import (
	"fmt"
	"path/filepath"
)

const (
	systemHelperSocketPath = "/var/run/tbx-helper.sock"
	helperSocketEnv        = "TBX_HELPER_SOCKET"
)

func socketPathOverride(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be an absolute path, got %q", helperSocketEnv, value)
	}
	return filepath.Clean(value), nil
}
