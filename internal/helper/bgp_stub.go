//go:build !darwin

package helper

import (
	"encoding/json"
	"errors"
)

var errBGPUnsupported = errors.New("BGP is only available on macOS")

func (s *Server) enableBGP(json.RawMessage) error  { return errBGPUnsupported }
func (s *Server) disableBGP(json.RawMessage) error { return errBGPUnsupported }
