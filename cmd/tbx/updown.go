package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/daemon"
)

const defaultConfigFile = "talosbox.yaml"

func (c cli) runUp(args []string) error {
	cfg, err := loadConfigFile(args, "up")
	if err != nil {
		return err
	}
	var actions []daemon.Action
	if err := c.call("up", cfg, &actions); err != nil {
		return err
	}
	return c.printActions(actions, map[daemon.ActionKind]string{
		daemon.ActionCreate: "created %s",
		daemon.ActionStart:  "started %s",
		daemon.ActionNone:   "%s is up to date",
	})
}

func (c cli) runDown(args []string) error {
	cfg, err := loadConfigFile(args, "down")
	if err != nil {
		return err
	}
	var actions []daemon.Action
	if err := c.call("down", cfg, &actions); err != nil {
		return err
	}
	return c.printActions(actions, map[daemon.ActionKind]string{
		daemon.ActionStop:    "stopped %s",
		daemon.ActionNone:    "%s is not running",
		daemon.ActionMissing: "%s does not exist (skipped)",
	})
}

func (c cli) printActions(actions []daemon.Action, wording map[daemon.ActionKind]string) error {
	for _, action := range actions {
		format, ok := wording[action.Kind]
		if !ok {
			format = string(action.Kind) + " %s"
		}
		if _, err := fmt.Fprintf(c.out, format+"\n", action.Cluster); err != nil {
			return err
		}
	}
	return nil
}

func loadConfigFile(args []string, verb string) (config.Config, error) {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	path := fs.String("f", defaultConfigFile, "path to talosbox.yaml")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return config.Config{}, fmt.Errorf("read %s: %w", *path, err)
	}
	return config.Parse(data)
}
