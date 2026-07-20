// Package daemon implements the local tbx daemon protocol and VM lifecycle.
package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

const DefaultTalosVersion = "v1.13.6"

// Request is one newline-delimited daemon request.
type Request struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args"`
}

// Response is one newline-delimited daemon response.
type Response struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
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
	var response Response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return Response{}, fmt.Errorf("read daemon response: %w", err)
	}
	return response, nil
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
