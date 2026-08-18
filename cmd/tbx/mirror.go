package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/randax/talos-box/internal/daemon"
)

func (c cli) runMirror(args []string) error {
	// Flags are separated from positionals before any verb judgement, the way
	// runBGP does it: `tbx mirror --anything` is a flag mistake, and mirror
	// takes no flags, so it earns the usage line rather than being reported as
	// an unknown mirror command named "--anything". The parser's own message
	// would name a flag this group never has, so the usage line is the whole
	// answer.
	flags := flag.NewFlagSet("mirror", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return errors.New(groupUsages["mirror"])
	}
	if len(positionals) > 0 && positionals[0] != "offline" {
		// A mistyped verb is named rather than answered with the bare usage, the
		// same way every other group answers one (#274).
		return unknownVerbError("mirror", positionals[0])
	}
	if len(positionals) == 0 || len(positionals) > 2 {
		return errors.New(groupUsages["mirror"])
	}

	op := "mirror.offline.get"
	var request any = struct{}{}
	if len(positionals) == 2 {
		switch positionals[1] {
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
	_, err = fmt.Fprintf(c.out, "mirror offline is %s\n", state)
	return err
}
