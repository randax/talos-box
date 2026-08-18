// Package daemon implements the local tbx daemon protocol and VM lifecycle.
package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/randax/talos-box/internal/talosversion"
)

// DefaultTalosVersion and MinTalosVersion mirror the pinned support window;
// internal/talosversion is the single source of truth.
const (
	DefaultTalosVersion = talosversion.Default
	MinTalosVersion     = talosversion.Min
)

// ProtocolVersion is the daemon wire version understood by this CLI/server
// pair. New request fields must negotiate support before relying on them.
// Version 4 volume-gates node.remove and honors its force field.
// Version 5 honors the per-cluster talos spec on up requests instead of
// applying the file-level block to every created cluster.
// Version 6 volume-gates snapshot.restore, honors its force field, and returns
// its snapshots wrapped in a warning-carrying status.
// Version 7 returns snapshot.create's snapshots wrapped in the same
// warning-carrying status, so the restart's host-subnet finding reaches the
// operator instead of only the daemon log.
// Version 8 serves node.start and node.stop, answers node.remove once the
// node is off the substrate instead of holding the request for the follow-up
// reconcile, and adds the additive suspended (cluster status) and incomplete
// (cache image entry) fields.
// Version 9 narrates the state-changing verbs: a request that sets progress
// receives stage responses on its own connection ahead of the single final
// one, and cluster.create holds its answer until the nodes it started have
// booted (#263 #273).
const ProtocolVersion = 9

// Request is one newline-delimited daemon request.
type Request struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args"`
	// Progress asks the daemon to narrate the operation's stages on this
	// connection while it runs. It is opt-in so a client that reads exactly
	// one response — an older tbx, or any of the non-narrating verbs — never
	// sees a message it would mistake for the result.
	Progress bool `json:"progress,omitempty"`
}

// Response is one newline-delimited daemon response. A response carrying a
// Stage is narration: the operation is still running and more responses
// follow. Every other response is the operation's single final answer.
type Response struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
	Stage string          `json:"stage,omitempty"`
}

// IsProgress reports whether this response is a narration stage rather than
// the operation's result.
func (r Response) IsProgress() bool { return r.Stage != "" }

// Info reports daemon wire compatibility details.
type Info struct {
	ProtocolVersion int `json:"protocolVersion"`
}

// SocketPath returns the per-user daemon socket path.
func SocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".talosbox", "tbxd.sock"), nil
}

// Call sends one request and waits for one response.
func Call(socketPath, op string, args any) (Response, error) {
	rawArgs, err := json.Marshal(args)
	if err != nil {
		return Response{}, fmt.Errorf("encode request arguments: %w", err)
	}
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = connection.Close() }()

	request := Request{Op: op, Args: rawArgs}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return Response{}, fmt.Errorf("write daemon request: %w", err)
	}
	decoder := json.NewDecoder(connection)
	for {
		var response Response
		if err := decoder.Decode(&response); err != nil {
			return Response{}, fmt.Errorf("read daemon response: %w", err)
		}
		// Call never asks for narration, but a stage response is still skipped
		// rather than returned as a result if one ever arrives.
		if response.IsProgress() {
			continue
		}
		return response, nil
	}
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
