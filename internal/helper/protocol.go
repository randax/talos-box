// Package helper implements the privileged talosbox helper protocol.
package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/randax/talos-box/internal/shellquote"
)

const (
	// Version 2 added dns.syncDomains and the domain argument (with helper-side
	// validation) on dns.listen/dns.register.
	//
	// Version 3 returns the speaker's injected routes from bgp.status (version 2
	// answered with {active} alone) and mirrors speaker FIB writes into the frame
	// router. A version 2 helper therefore reports no routes for a speaker that is
	// announcing, and leaves the cross-cluster BGP-VIP path dead; refusing the
	// handshake sends the operator to a reinstall instead of a wrong answer.
	//
	// Version 4 added net.teardown, which removes the br-tbx<n> bridge (and with
	// it the gateway address) when the last cluster on a subnet is destroyed. A
	// version 3 helper has no such op, so every destroy leaks the bridge and the
	// next create climbs to a fresh subnet index; refusing the handshake sends
	// the operator to a reinstall instead of accumulating host residue.
	//
	// Version 5 added net.sync, which pushes the cluster reservations tbxd owns
	// into the helper. The packaged Linux helper runs as an unprivileged system
	// user that cannot read a caller's home, so without this op it serves no
	// DHCP and reconverges no bridges; a version 4 helper would silently leave
	// every cluster without addresses.
	//
	// Bumping this also means bumping the literal in nix/vm-test.nix: the NixOS
	// smoke test's helper-probe performs this handshake against the packaged
	// helper, and `nix flake check` fails on a mismatch.
	protocolVersion = 5
	helperInfoOp    = "helper.info"
)

var errProtocolMismatch = errors.New("helper protocol mismatch")

// linuxHelperReinstallAdvice is the Linux recovery for a stale helper. Linux
// installation is owned by packages and systemd units — `tbx system install`
// installs the macOS launchd helper there, which docs/linux.md forbids — so the
// advice names the package upgrade and the socket restart instead.
const linuxHelperReinstallAdvice = "reinstall the helper: upgrade the tbx-helper package " +
	"(or reinstall the binary and units as in docs/linux.md, \"Build and install the source preview\"), " +
	"then run `sudo systemctl restart tbx-helper.socket`"

// The installed helper binary is pinned at an absolute path by its service
// definition, so restarting it relaunches the same stale binary; only a
// reinstall replaces it. Which reinstall depends on the host's install
// mechanism, so the advice is platform-branched.
func protocolMismatchAdvice() string {
	executable, err := os.Executable()
	return protocolMismatchAdviceForGOOS(runtime.GOOS, executable, err)
}

func protocolMismatchAdviceForGOOS(goos, executable string, lookupErr error) string {
	if goos == "linux" {
		return linuxHelperReinstallAdvice
	}
	return protocolMismatchAdviceFor(executable, lookupErr)
}

// protocolMismatchAdviceFor is the launchd-install advice. It names the
// concrete tbx path because sudo does not resolve a checkout's bin directory:
// tbx and tbxd sit next to each other, so the running executable's directory
// locates tbx from either.
func protocolMismatchAdviceFor(executable string, lookupErr error) string {
	if lookupErr != nil {
		return "run `sudo tbx system install` from the current checkout to reinstall the helper"
	}
	tbxPath := shellquote.Quote(filepath.Join(filepath.Dir(executable), "tbx"))
	return fmt.Sprintf("run `sudo %s system install` to reinstall the helper", tbxPath)
}

// Request is one newline-delimited helper request.
type Request struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args"`
}

// Response is one newline-delimited helper response.
type Response struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Info describes the connected helper process.
type Info struct {
	ProtocolVersion          int      `json:"protocolVersion"`
	EffectiveCapabilities    uint64   `json:"effectiveCapabilities,omitempty"`
	EffectiveCapabilityNames []string `json:"effectiveCapabilityNames,omitempty"`
}

func protocolMismatchError(clientVersion, helperVersion int) error {
	return fmt.Errorf(
		"%w (client %d, helper %d): %s",
		errProtocolMismatch,
		clientVersion,
		helperVersion,
		protocolMismatchAdvice(),
	)
}

func protocolHandshakeFailure(detail string) error {
	if detail == "" {
		detail = "helper rejected the version handshake"
	}
	// A mismatched helper rejects the handshake with its own mismatch message,
	// whose advice reflects that stale build (any wording, and any path it
	// resolved for itself). Keep its version facts — everything before the
	// colon — substitute this build's advice, and wrap once instead of
	// nesting mismatch inside mismatch.
	if rest, ok := strings.CutPrefix(detail, errProtocolMismatch.Error()); ok {
		facts, _, _ := strings.Cut(rest, ":")
		facts = strings.TrimRight(facts, ";: ")
		return fmt.Errorf("%w%s: %s", errProtocolMismatch, facts, protocolMismatchAdvice())
	}
	return fmt.Errorf("%w: %s; %s", errProtocolMismatch, detail, protocolMismatchAdvice())
}

func success(data any) Response {
	raw, err := json.Marshal(data)
	if err != nil {
		return failure(fmt.Errorf("encode response: %w", err))
	}
	return Response{OK: true, Data: raw}
}

func failure(err error) Response {
	return Response{OK: false, Error: err.Error()}
}

// UnavailableAdvice is what to tell an operator whose helper cannot be reached
// at all — a different failure from a stale helper, which protocolMismatch-
// Advice covers. Every caller shares this so no platform ever reads another
// platform's remediation: on Linux the helper is a packaged systemd socket
// unit gated on the `tbx` group, and `tbx system install` (which installs the
// macOS launchd helper) must never be printed there (#468).
func UnavailableAdvice() string {
	return unavailableAdviceForGOOS(runtime.GOOS)
}

func unavailableAdviceForGOOS(goos string) string {
	if goos == "linux" {
		return "enable the helper: `sudo systemctl enable --now tbx-helper.socket` " +
			"and add your user to the `tbx` group (docs/linux.md)"
	}
	return "run `sudo tbx system install`"
}
