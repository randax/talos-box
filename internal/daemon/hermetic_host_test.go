package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/helper"
)

// TestMain pins the host readings for the whole package. Any test that reaches
// checkProvisionStart without stubbing them would otherwise be measuring the
// runner it happens to be on: a busy laptop or a small CI box has less free
// memory than the balloon reserve, and the start gate then refuses a create the
// test was not written to be about. Generous, deterministic values keep the
// gate open by default; a test that is *about* the gate sets the Server's own
// hostFreeMemory/hostTotalMemory/hostPressure, which always win over these.
func TestMain(m *testing.M) {
	measureHostFreeMiB = plentifulHostMemory
	measureHostTotalMiB = plentifulHostMemory
	measureHostPressure = noHostPressure
	// The BGP port inventory shells out to the host's netstat, whose listeners
	// are the runner's business and not any test's: an unrelated process on
	// port 179 must not add an advisory to a `bgp enable` a test is asserting
	// on. Tests about the inventory stub it themselves.
	bgpPortListeners = nil
	// Status must never inherit a developer's talosconfig or dial a real node.
	// Tests about service state replace these seams explicitly, as #355's host
	// readings do above.
	lookupNodeTalosContext = func(string) (string, *clientconfig.Context, error) {
		return "", nil, errTalosContextMissing
	}
	listNodeServices = func(context.Context, *clientconfig.Context, string) (*machineapi.ServiceListResponse, error) {
		return nil, errors.New("live Talos service list disabled in package tests")
	}
	probeNodeServices = func(string, string, time.Time) ([]NodeService, ServiceProbe) {
		return nil, ServiceProbe{Status: ServiceProbeMissingCredentials}
	}
	socketDir, err := containHostHelper()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(socketDir)
	os.Exit(code)
}

// containHostHelper keeps the package's tests off the real host helper. The
// helper's client socket is resolved from /run/user/<uid> on Linux (or
// /var/run elsewhere), never from $HOME, so the HOME isolation the tests rely
// on does not reach it: on a host with the helper installed, a destroy test
// would send a live cluster's dns.unregister and net.teardown, and a BGP test
// its bgp.disable, to the running host (#445 follow-up). Two pins, because the
// helper is reached from more than one place:
//
//   - TBX_HELPER_SOCKET points every helper.Connect in the package at a path
//     nothing binds, so a connection can only fail. That covers the call sites
//     with no seam of their own — requireHelper, the resolver-file sync, and
//     the host BGP disable, which leaked before this change too.
//   - connectSyncHelper answers as a helper that accepts every reservation
//     push, so the start paths — which refuse to launch a node the helper
//     could not be told about — behave as they do beside a running helper. A
//     test about the push installs its own recording client.
//   - connectBridgeHelper answers as a host that has no bridge for the subnet,
//     so the destroy path stays quiet on every GOOS instead of decorating
//     every summary with a connection-failure warning. A test about the
//     release installs its own recording client.
func containHostHelper() (string, error) {
	dir, err := os.MkdirTemp("", "tbx-daemon-helper")
	if err != nil {
		return "", fmt.Errorf("helper socket sandbox: %w", err)
	}
	// helper's own env name, spelled out because the constant is unexported.
	if err := os.Setenv("TBX_HELPER_SOCKET", filepath.Join(dir, "unbound-helper.sock")); err != nil {
		return dir, fmt.Errorf("pin TBX_HELPER_SOCKET: %w", err)
	}
	connectBridgeHelper = func() (bridgeReleaseHelper, error) { return noBridgeHelper{}, nil }
	connectSyncHelper = func() (helperSyncClient, error) { return acceptingSyncHelper{}, nil }
	return dir, nil
}

// noBridgeHelper answers like a host that never built the subnet's bridge:
// nothing to withdraw, nothing to take down.
type noBridgeHelper struct{}

func (noBridgeHelper) UnregisterDNS(int) error          { return nil }
func (noBridgeHelper) TeardownSubnet(int) (bool, error) { return false, nil }
func (noBridgeHelper) Close() error                     { return nil }

// acceptingSyncHelper answers like a helper that accepted the reservations.
type acceptingSyncHelper struct{}

func (acceptingSyncHelper) Sync([]cluster.Cluster) error { return nil }
func (acceptingSyncHelper) Close() error                 { return nil }

// The containment is only worth as much as its proof: if a refactor drops the
// TBX_HELPER_SOCKET pin, this fails here instead of the next Linux runner
// losing a live cluster's DNS registration and bridge to a destroy test.
func TestPackageTestsCannotReachTheHostHelper(t *testing.T) {
	client, err := helper.Connect()
	if err == nil {
		_ = client.Close()
		t.Fatal("helper.Connect() reached a helper from a package test; the TBX_HELPER_SOCKET pin is gone")
	}
}
