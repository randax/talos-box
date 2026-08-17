package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type mirrorOfflineArgs struct {
	Enabled *bool `json:"enabled"`
}

func (s *Server) getMirrorOffline() (MirrorOfflineStatus, error) {
	if s.mirrors != nil {
		return MirrorOfflineStatus{Enabled: s.mirrors.Offline()}, nil
	}
	return MirrorOfflineStatus{Enabled: s.mirrorOffline.Load()}, nil
}

func (s *Server) setMirrorOffline(raw json.RawMessage) (MirrorOfflineStatus, error) {
	args, err := decodeMirrorOfflineArgs(raw)
	if err != nil {
		return MirrorOfflineStatus{}, err
	}
	// Persist before applying: a mode the daemon reports as on, but would
	// forget on the next restart, is the bug this guards (#318).
	if s.settingsPath != "" {
		if err := updateSettings(s.settingsPath, func(current *settings) {
			current.MirrorOffline = *args.Enabled
		}); err != nil {
			return MirrorOfflineStatus{}, err
		}
	}
	s.mirrorOffline.Store(*args.Enabled)
	if s.mirrors != nil {
		s.mirrors.SetOffline(*args.Enabled)
	}
	return MirrorOfflineStatus{Enabled: *args.Enabled}, nil
}

func decodeMirrorOfflineArgs(raw json.RawMessage) (mirrorOfflineArgs, error) {
	if len(raw) == 0 {
		return mirrorOfflineArgs{}, errors.New("decode arguments: enabled is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return mirrorOfflineArgs{}, fmt.Errorf("decode arguments: %w", err)
	}
	objectStart, ok := token.(json.Delim)
	if !ok || objectStart != '{' {
		return mirrorOfflineArgs{}, errors.New("decode arguments: expected object")
	}
	var args mirrorOfflineArgs
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return mirrorOfflineArgs{}, fmt.Errorf("decode arguments: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return mirrorOfflineArgs{}, errors.New("decode arguments: expected object key")
		}
		if key != "enabled" {
			return mirrorOfflineArgs{}, fmt.Errorf("decode arguments: unknown field %q", key)
		}
		if args.Enabled != nil {
			return mirrorOfflineArgs{}, errors.New("decode arguments: duplicate enabled field")
		}
		var rawValue json.RawMessage
		if err := decoder.Decode(&rawValue); err != nil {
			return mirrorOfflineArgs{}, fmt.Errorf("decode arguments: %w", err)
		}
		switch string(rawValue) {
		case "true":
			enabled := true
			args.Enabled = &enabled
		case "false":
			enabled := false
			args.Enabled = &enabled
		default:
			return mirrorOfflineArgs{}, errors.New("decode arguments: enabled must be boolean")
		}
	}
	endToken, err := decoder.Token()
	if err != nil {
		return mirrorOfflineArgs{}, fmt.Errorf("decode arguments: %w", err)
	}
	objectEnd, ok := endToken.(json.Delim)
	if !ok || objectEnd != '}' {
		return mirrorOfflineArgs{}, errors.New("decode arguments: expected object end")
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return mirrorOfflineArgs{}, errors.New("decode arguments: trailing data")
		}
		return mirrorOfflineArgs{}, fmt.Errorf("decode arguments: %w", err)
	}
	if args.Enabled == nil {
		return mirrorOfflineArgs{}, errors.New("decode arguments: enabled is required")
	}
	return args, nil
}
