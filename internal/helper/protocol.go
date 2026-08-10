// Package helper implements the privileged talosbox helper protocol.
package helper

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// Version 2 added dns.syncDomains and the domain argument (with helper-side
	// validation) on dns.listen/dns.register.
	protocolVersion = 2
	helperInfoOp    = "helper.info"
)

var errProtocolMismatch = errors.New("helper protocol mismatch")

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
		"%w (client %d, helper %d): restart the helper",
		errProtocolMismatch,
		clientVersion,
		helperVersion,
	)
}

func protocolHandshakeFailure(detail string) error {
	if detail == "" {
		detail = "helper rejected the version handshake"
	}
	return fmt.Errorf("%w: %s; restart the helper", errProtocolMismatch, detail)
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
