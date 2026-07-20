// Package helper implements the privileged talosbox helper protocol.
package helper

import (
	"encoding/json"
	"fmt"
)

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
