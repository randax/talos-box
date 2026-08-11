package daemon

import "encoding/json"

type mirrorOfflineArgs struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) getMirrorOffline() (MirrorOfflineStatus, error) {
	if s.mirrors != nil {
		return MirrorOfflineStatus{Enabled: s.mirrors.Offline()}, nil
	}
	return MirrorOfflineStatus{Enabled: s.mirrorOffline.Load()}, nil
}

func (s *Server) setMirrorOffline(raw json.RawMessage) (MirrorOfflineStatus, error) {
	var args mirrorOfflineArgs
	if err := decodeArgs(raw, &args); err != nil {
		return MirrorOfflineStatus{}, err
	}
	s.mirrorOffline.Store(args.Enabled)
	if s.mirrors != nil {
		s.mirrors.SetOffline(args.Enabled)
	}
	return MirrorOfflineStatus(args), nil
}
