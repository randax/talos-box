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
// Version 10 reconciles the CNI on bgp.enable/bgp.disable: the mode change
// re-renders Cilium with the BGP control plane on or off, rolls its agents and
// applies the matching announcement objects before answering, instead of moving
// only the host speaker (#344). It also adds the additive storagePending field
// to cluster status, which separates a readiness probe the daemon's own cleanup
// has not let run yet from one that failed (#347).
// Version 11 adds the additive per-node kubelet field to cluster status: the
// node's kubelet as its machine API reports it, so a node that answers apid
// with a crashlooping kubelet no longer reads as a healthy configured node
// (#357).
// Version 12 adds the additive noop field to the node.start/node.stop
// response: a verb that found the node already in the requested run state
// changed nothing, and the CLI says so instead of claiming it acted (#362).
// Version 13 honors the additive force field on cluster.resume: a resume
// re-admits the suspended cluster's whole memory footprint, so it answers to
// the same host-pressure gate create and start do, with --force as the
// override (#368).
// Version 14 serves bgp.status: a read-only report of a cluster's announcement
// mode, the host speaker behind it and the routes that speaker announces, so a
// refused or deferred mode change can be confirmed directly instead of through
// `tbx doctor` (#399).
const ProtocolVersion = 14

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

// NodeRunState is the answer to node.start and node.stop: the node's status
// plus whether the verb actually changed anything. The status is embedded, so
// the wire shape stays the plain NodeStatus object an older tbx already reads,
// with one additive field beside it.
type NodeRunState struct {
	NodeStatus
	// NoOp reports that the node was already in the requested run state, so
	// nothing was started or stopped. The request still succeeds — asking for
	// a state the node is already in is not an error — but the CLI narrates it
	// honestly rather than claiming an action it never took (#362).
	NoOp bool `json:"noop,omitempty"`
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
