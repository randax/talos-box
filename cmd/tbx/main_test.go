package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/hostpressure"
)

// TestMain pins the runtime-identity and daemon-launch seams so no test depends
// on the host's systemd state. Tests that care about a systemd answer override
// the relevant seam locally and restore it with cleanup.
func TestMain(m *testing.M) {
	pinPlatformLaunchSeams()
	statusMemorySnapshot = func() (hostpressure.Snapshot, error) {
		return hostpressure.Snapshot{TotalMemoryMiB: 32768, FreeMemoryMiB: 16384, MemoryPressure: hostpressure.MemoryPressureNormal}, nil
	}
	runtimeIdentityCommand = func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 0 && (args[0] == "show" || (args[0] == "--user" && len(args) > 1 && args[1] == "show")) {
			return []byte("LoadState=not-found\nActiveState=inactive\n"), nil
		}
		return nil, fmt.Errorf("runtime identity command not stubbed in tests: %s %s", name, strings.Join(args, " "))
	}
	os.Exit(m.Run())
}
