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
	cfg, force, err := loadUpConfigFile(args)
	if err != nil {
		return err
	}
	var actions []daemon.Action
	request := struct {
		config.Config
		Force bool `json:"force"`
	}{Config: cfg, Force: force}
	if err := c.call("up", request, &actions); err != nil {
		return err
	}
	return c.printActions(actions, map[daemon.ActionKind]string{
		daemon.ActionCreate:    "created %s",
		daemon.ActionStart:     "started %s",
		daemon.ActionReconcile: "reconciled %s",
		daemon.ActionNone:      "%s is up to date",
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
		if err := printWarning(c.err, action.Warning); err != nil {
			return err
		}
		for _, line := range action.Narration {
			if _, err := fmt.Fprintln(c.out, line); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadUpConfigFile(args []string) (config.Config, bool, error) {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	path := fs.String("f", defaultConfigFile, "path to talosbox.yaml")
	force := fs.Bool("force", false, "override overcommit or host-pressure safeguards")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, false, err
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return config.Config{}, false, fmt.Errorf("read %s: %w", *path, err)
	}
	cfg, err := config.Parse(data)
	return cfg, *force, err
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
