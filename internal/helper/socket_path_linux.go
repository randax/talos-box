//go:build linux

package helper

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// SocketPath returns the path used by an invoking helper client. Root clients
// launched through sudo follow SUDO_UID to the authorized user's runtime socket.
func SocketPath() (string, error) {
	override := os.Getenv(helperSocketEnv)
	path, err := linuxClientSocketPath(uint32(os.Geteuid()), os.Getenv("SUDO_UID"), override)
	return validateLinuxSocketPath(path, override, err)
}

// ServerSocketPath returns the path the helper must bind for its authorized
// client. Root-only helpers retain the system path; user helpers and sudo
// launches use the authorized user's runtime directory.
func ServerSocketPath(allowedUID *uint32) (string, error) {
	override := os.Getenv(helperSocketEnv)
	path, err := linuxServerSocketPath(uint32(os.Geteuid()), allowedUID, override)
	return validateLinuxSocketPath(path, override, err)
}

func socketPathOverride(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be an absolute path, got %q", helperSocketEnv, value)
	}
	return filepath.Clean(value), nil
}

func linuxClientSocketPath(effectiveUID uint32, sudoUID, override string) (string, error) {
	if path, err := socketPathOverride(override); path != "" || err != nil {
		return path, err
	}
	if effectiveUID != 0 {
		return linuxUserSocketPath(effectiveUID), nil
	}
	if parsed, err := strconv.ParseUint(sudoUID, 10, 32); err == nil && parsed != 0 {
		return linuxUserSocketPath(uint32(parsed)), nil
	}
	return systemHelperSocketPath, nil
}

func linuxServerSocketPath(effectiveUID uint32, allowedUID *uint32, override string) (string, error) {
	if path, err := socketPathOverride(override); path != "" || err != nil {
		return path, err
	}
	if allowedUID != nil && *allowedUID != 0 {
		return linuxUserSocketPath(*allowedUID), nil
	}
	if effectiveUID != 0 {
		return linuxUserSocketPath(effectiveUID), nil
	}
	return systemHelperSocketPath, nil
}

func linuxUserSocketPath(uid uint32) string {
	return filepath.Join("/run/user", fmt.Sprint(uid), "tbx-helper.sock")
}

func validateLinuxSocketPath(path, override string, pathErr error) (string, error) {
	if pathErr != nil || override != "" || path == systemHelperSocketPath {
		return path, pathErr
	}
	runtimeDir := filepath.Dir(path)
	info, err := os.Stat(runtimeDir)
	if err != nil {
		return "", fmt.Errorf("Linux runtime directory %s is unavailable: %w; set %s to an accessible absolute path", runtimeDir, err, helperSocketEnv)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Linux runtime path %s is not a directory; set %s to an accessible absolute path", runtimeDir, helperSocketEnv)
	}
	return path, nil
}
