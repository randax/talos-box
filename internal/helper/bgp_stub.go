//go:build !darwin && !linux

package helper

import (
	"encoding/json"
	"errors"
)

var errBGPUnsupported = errors.New("BGP is only available on macOS and Linux")

func (s *Server) enableBGP(json.RawMessage) error  { return errBGPUnsupported }
func (s *Server) disableBGP(json.RawMessage) error { return errBGPUnsupported }
