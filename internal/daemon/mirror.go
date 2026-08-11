package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	s.mirrorOffline.Store(*args.Enabled)
	if s.mirrors != nil {
		s.mirrors.SetOffline(*args.Enabled)
	}
	return MirrorOfflineStatus{Enabled: *args.Enabled}, nil
}

func decodeMirrorOfflineArgs(raw json.RawMessage) (mirrorOfflineArgs, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var args mirrorOfflineArgs
	if err := decoder.Decode(&args); err != nil {
		return mirrorOfflineArgs{}, fmt.Errorf("decode arguments: %w", err)
	}
	if args.Enabled == nil {
		return mirrorOfflineArgs{}, errors.New("decode arguments: enabled is required")
	}
	return args, nil
}
