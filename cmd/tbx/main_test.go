package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestMain pins the runtime-identity command seam so no test depends on the
// host's systemd state: on a Linux runner `systemctl show tbxd.service` would
// otherwise decide whether a fake daemon socket is dialed at all. Tests that
// care about the systemd answer inject their own command (see
// runtime_identity_linux_test.go).
func TestMain(m *testing.M) {
	runtimeIdentityCommand = func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 0 && (args[0] == "show" || (args[0] == "--user" && len(args) > 1 && args[1] == "show")) {
			return []byte("LoadState=not-found\nActiveState=inactive\n"), nil
		}
		return nil, fmt.Errorf("runtime identity command not stubbed in tests: %s %s", name, strings.Join(args, " "))
	}
	os.Exit(m.Run())
}
