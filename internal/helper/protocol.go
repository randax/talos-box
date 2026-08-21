// Package helper implements the privileged talosbox helper protocol.
package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// Bumping this also means bumping the literal in nix/vm-test.nix: the NixOS
	// smoke test's helper-probe performs this handshake against the packaged
	// helper, and `nix flake check` fails on a mismatch.
	protocolVersion = 3
	helperInfoOp    = "helper.info"
)

var errProtocolMismatch = errors.New("helper protocol mismatch")

// The installed helper binary is pinned at an absolute path by its service
// definition, so restarting it relaunches the same stale binary; only a
// reinstall replaces it. The advice names the concrete tbx path because sudo
// does not resolve a checkout's bin directory: tbx and tbxd sit next to each
// other, so the running executable's directory locates tbx from either.
func protocolMismatchAdvice() string {
	executable, err := os.Executable()
	return protocolMismatchAdviceFor(executable, err)
}

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
