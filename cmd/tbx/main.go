package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/imagecache"
	"github.com/randax/talos-box/internal/version"
)

type cli struct {
	out io.Writer
	err io.Writer
	in  io.Reader
}

func main() {
	command := cli{out: os.Stdout, err: os.Stderr, in: os.Stdin}
	if err := command.run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "tbx: %v\n", err)
		os.Exit(1)
	}
}

func (c cli) run(args []string) error {
	if len(args) == 0 {
		c.printHelp(c.out)
		return nil
	}
	switch args[0] {
	case "cluster":
		return c.runCluster(args[1:])
	case "node":
		return c.runNode(args[1:])
	case "up":
		return c.runUp(args[1:])
	case "down":
		return c.runDown(args[1:])
	case "status":
		return c.runStatus(args[1:])
	case "manifests":
		return c.runManifests(args[1:])
	case "bgp":
		return c.runBGP(args[1:])
	case "snapshot":
		return c.runSnapshot(args[1:])
	case "console":
		return c.runConsole(args[1:])
	case "cache":
		return c.runCache(args[1:])
	case "system":
		return c.runSystem(args[1:])
	case "doctor":
		return c.runDoctor(args[1:])
	case "version":
		_, err := fmt.Fprintln(c.out, version.Version)
		return err
	case "help", "-h", "--help":
		c.printHelp(c.out)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (c cli) runCluster(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tbx cluster create|start|stop|destroy|list")
	}
	switch args[0] {
	case "create":
		return c.createCluster(args[1:])
	case "start", "stop", "suspend", "resume":
		if len(args) != 2 {
			return fmt.Errorf("usage: tbx cluster %s <name>", args[0])
		}
		var result daemon.ClusterSummary
		if err := c.call("cluster."+args[0], map[string]string{"name": args[1]}, &result); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.out, "%s cluster %s\n", pastTense(args[0]), result.Name); err != nil {
			return err
		}
		return printWarning(c.err, result.Warning)
	case "destroy":
		return c.destroyCluster(args[1:])
	case "list":
		flags := flag.NewFlagSet("cluster list", flag.ContinueOnError)
		flags.SetOutput(c.err)
		outputFormat := flags.String("o", "table", "output format: table|json")
		positionals, err := parseInterspersed(flags, args[1:])
		if err != nil {
			return err
		}
		if len(positionals) != 0 {
			return errors.New("usage: tbx cluster list [-o json]")
		}
		var result []daemon.ClusterSummary
		if err := c.call("cluster.list", struct{}{}, &result); err != nil {
			return err
		}
		if *outputFormat == "json" {
			return encodeJSON(c.out, result)
		}
		return printClusters(c.out, result)
	default:
		return fmt.Errorf("unknown cluster command %q", args[0])
	}
}

func (c cli) createCluster(args []string) error {
	flags := flag.NewFlagSet("cluster create", flag.ContinueOnError)
	flags.SetOutput(c.err)
	controlPlanes := flags.Int("cp", 1, "number of control planes")
	workers := flags.Int("workers", 2, "number of workers")
	memory := flags.Int("memory-mib", cluster.DefaultMemoryMiB, "memory per node in MiB")
	cpus := flags.Int("cpus", cluster.DefaultCPUs, "CPUs per node")
	disk := flags.Int("disk-gib", cluster.DefaultDiskGiB, "disk size per node in GiB")
	talosVersion := flags.String("talos-version", daemon.DefaultTalosVersion, "Talos version")
	schematic := flags.String("schematic", "", "Image Factory schematic")
	force := flags.Bool("force", false, "proceed despite an overcommit warning")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return errors.New("usage: tbx cluster create <name> [--cp N --workers N]")
	}
	resolvedSchematic, err := resolveSchematic(*schematic)
	if err != nil {
		return err
	}
	request := struct {
		Name          string               `json:"name"`
		ControlPlanes int                  `json:"controlPlanes"`
		Workers       int                  `json:"workers"`
		Node          cluster.NodeDefaults `json:"node"`
		Force         bool                 `json:"force"`
		Schematic     string               `json:"schematic"`
		Version       string               `json:"version"`
	}{
		Name: positionals[0], ControlPlanes: *controlPlanes, Workers: *workers,
		Node:  cluster.NodeDefaults{MemoryMiB: *memory, CPUs: *cpus, DiskGiB: *disk},
		Force: *force, Schematic: resolvedSchematic, Version: *talosVersion,
	}
	var result daemon.ClusterSummary
	if err := c.call("cluster.create", request, &result); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.out, "created and started cluster %s (%d control plane, %d workers)\n",
		result.Name, result.ControlPlanes, result.Workers); err != nil {
		return err
	}
	if err := printWarning(c.err, result.Warning); err != nil {
		return err
	}
	stanza := config.Marshal(config.Config{
		Talos: config.TalosSpec{Version: result.TalosVersion, Schematic: result.Schematic},
		Clusters: []config.ClusterSpec{{
			Name: result.Name, ControlPlanes: result.ControlPlanes, Workers: result.Workers,
			Node: result.NodeDefaults,
		}}})
	_, err = fmt.Fprintf(c.out, "\nequivalent talosbox.yaml:\n%s", stanza)
	return err
}

func (c cli) destroyCluster(args []string) error {
	flags := flag.NewFlagSet("cluster destroy", flag.ContinueOnError)
	flags.SetOutput(c.err)
	force := flags.Bool("force", false, "confirm permanent deletion")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return errors.New("usage: tbx cluster destroy <name> --force")
	}
	if !*force {
		return errors.New("cluster destroy requires --force")
	}
	request := struct {
		Name  string `json:"name"`
		Force bool   `json:"force"`
	}{Name: positionals[0], Force: true}
	if err := c.call("cluster.destroy", request, nil); err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.out, "destroyed cluster %s\n", positionals[0])
	return err
}

func (c cli) runNode(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tbx node add|remove <cluster> [node]")
	}
	switch args[0] {
	case "add":
		flags := flag.NewFlagSet("node add", flag.ContinueOnError)
		flags.SetOutput(c.err)
		role := flags.String("role", string(cluster.RoleWorker), "worker or control-plane")
		force := flags.Bool("force", false, "proceed despite an overcommit warning")
		positionals, err := parseInterspersed(flags, args[1:])
		if err != nil {
			return err
		}
		if len(positionals) < 1 || len(positionals) > 2 {
			return errors.New("usage: tbx node add <cluster> [node] [--role worker|control-plane] [--force]")
		}
		name := ""
		if len(positionals) == 2 {
			name = positionals[1]
		}
		request := map[string]any{"cluster": positionals[0], "name": name, "role": *role, "force": *force}
		var result daemon.NodeStatus
		if err := c.call("node.add", request, &result); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.out, "added node %s to cluster %s\n", result.Name, positionals[0]); err != nil {
			return err
		}
		return printWarning(c.err, result.Warning)
	case "remove":
		if len(args) != 3 {
			return errors.New("usage: tbx node remove <cluster> <node>")
		}
		request := map[string]string{"cluster": args[1], "name": args[2]}
		if err := c.call("node.remove", request, nil); err != nil {
			return err
		}
		_, err := fmt.Fprintf(c.out, "removed node %s from cluster %s\n", args[2], args[1])
		return err
	default:
		return fmt.Errorf("unknown node command %q", args[0])
	}
}

func (c cli) runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(c.err)
	quiet := flags.Bool("quiet", false, "suppress hints")
	outputFormat := flags.String("o", "table", "output format: table|json")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) > 1 {
		return errors.New("usage: tbx status [cluster] [--quiet] [-o json]")
	}
	name := ""
	if len(positionals) == 1 {
		name = positionals[0]
	}
	var result []daemon.ClusterStatus
	if err := c.call("status", map[string]string{"cluster": name}, &result); err != nil {
		return err
	}
	if *quiet {
		for i := range result {
			result[i].Hints = nil
		}
	}
	if *outputFormat == "json" {
		return encodeJSON(c.out, result)
	}
	return printStatus(c.out, result, *quiet)
}

func (c cli) runCache(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tbx cache pull|prune")
	}
	switch args[0] {
	case "pull":
		flags := flag.NewFlagSet("cache pull", flag.ContinueOnError)
		flags.SetOutput(c.err)
		talosVersion := flags.String("talos-version", daemon.DefaultTalosVersion, "Talos version")
		schematic := flags.String("schematic", "", "Image Factory schematic")
		positionals, err := parseInterspersed(flags, args[1:])
		if err != nil {
			return err
		}
		if len(positionals) != 0 {
			return errors.New("usage: tbx cache pull [--talos-version VERSION --schematic ID]")
		}
		resolvedSchematic, err := resolveSchematic(*schematic)
		if err != nil {
			return err
		}
		request := map[string]string{"version": *talosVersion, "schematic": resolvedSchematic}
		var result daemon.CachePullResult
		if err := c.call("cache.pull", request, &result); err != nil {
			return err
		}
		_, err = fmt.Fprintf(c.out, "cached Talos %s schematic %s at %s\n", result.Version, result.Schematic, result.Path)
		return err
	case "prune":
		if len(args) != 1 {
			return errors.New("usage: tbx cache prune")
		}
		var result struct {
			Removed int `json:"removed"`
		}
		if err := c.call("cache.prune", struct{}{}, &result); err != nil {
			return err
		}
		_, err := fmt.Fprintf(c.out, "pruned %d cached image(s)\n", result.Removed)
		return err
	default:
		return fmt.Errorf("unknown cache command %q", args[0])
	}
}

func (c cli) printHelp(output io.Writer) {
	const help = `Usage: tbx <command>

Commands:
  cluster create|start|stop|destroy|list
  node add|remove
  status [cluster]
  cache pull|prune
  system install|uninstall
  doctor
  version
`
	_, _ = fmt.Fprint(output, help)
}

func printWarning(w io.Writer, warning string) error {
	if warning == "" {
		return nil
	}
	_, err := fmt.Fprintf(w, "warning: %s\n", warning)
	return err
}

func pastTense(command string) string {
	switch command {
	case "stop":
		return "stopped"
	case "suspend":
		return "suspended"
	case "resume":
		return "resumed"
	default:
		return "started"
	}
}

func resolveSchematic(schematic string) (string, error) {
	if schematic != "" {
		return schematic, nil
	}
	cache, err := imagecache.NewDefault()
	if err != nil {
		return "", err
	}
	return cache.Schematic()
}
