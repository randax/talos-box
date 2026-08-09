//go:build linux

package helper

import (
	"encoding/json"
	"errors"
)

var errBGPUnsupported = errors.New("BGP helper support is not implemented on Linux")

func (s *Server) enableBGP(json.RawMessage) error  { return errBGPUnsupported }
func (s *Server) disableBGP(json.RawMessage) error { return errBGPUnsupported }
