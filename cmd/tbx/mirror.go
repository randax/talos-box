package main

import (
	"errors"
	"fmt"

	"github.com/randax/talos-box/internal/daemon"
)

func (c cli) runMirror(args []string) error {
	if len(args) > 0 && args[0] != "offline" {
		// A mistyped verb is named rather than answered with the bare usage, the
		// same way every other group answers one (#274).
		return unknownVerbError("mirror", args[0])
	}
	if len(args) == 0 || len(args) > 2 {
		return errors.New(groupUsages["mirror"])
	}

	op := "mirror.offline.get"
	var request any = struct{}{}
	if len(args) == 2 {
		switch args[1] {
		case "on":
			op = "mirror.offline.set"
			request = struct {
				Enabled bool `json:"enabled"`
			}{Enabled: true}
		case "off":
			op = "mirror.offline.set"
			request = struct {
				Enabled bool `json:"enabled"`
			}{Enabled: false}
		default:
			return errors.New(groupUsages["mirror"])
		}
	}

	var result daemon.MirrorOfflineStatus
	if err := c.call(op, request, &result); err != nil {
		return err
	}
	state := "off"
	if result.Enabled {
		state = "on"
	}
	_, err := fmt.Fprintf(c.out, "mirror offline is %s\n", state)
	return err
}
