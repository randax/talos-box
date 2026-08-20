package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

func (c cli) runSnapshot(args []string) error {
	if len(args) == 0 {
		return errors.New(groupUsages["snapshot"])
	}
	switch args[0] {
	case "create":
		return c.snapshotCreate(args[1:])
	case "restore":
		return c.snapshotRestore(args[1:])
	case "delete":
		return c.snapshotDelete(args[1:])
	case "list":
		return c.snapshotList(args[1:])
	default:
		return unknownVerbError("snapshot", args[0])
	}
}

func (c cli) snapshotCreate(args []string) error {
	fs := flag.NewFlagSet("snapshot create", flag.ContinueOnError)
	fs.SetOutput(c.err)
	yes := fs.Bool("yes", false, "skip the running-cluster confirmation")
	quiet := fs.Bool("quiet", false, "suppress stage narration")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 || len(rest) > 2 {
		return errors.New("usage: tbx snapshot create <cluster> [name] [--yes] [--quiet]")
	}
	name := defaultSnapshotName()
	if len(rest) == 2 {
		name = rest[1]
	}
	if err := c.ensureSnapshotCreateSupport(); err != nil {
		return err
	}
	if err := c.confirmIfRunning(rest[0], *yes, "snapshot"); err != nil {
		return err
	}
	var result daemon.SnapshotStatus
	// A snapshot stops the whole cluster, clones every node disk and restarts
	// it; unnarrated that is a silent second of nothing (#273 #244).
	if err := c.callNarrated("snapshot.create", map[string]string{"cluster": rest[0], "name": name}, &result, c.stages(*quiet)); err != nil {
		return err
	}
	if err := printWarnings(c.err, result.Warnings, result.Warning); err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.out, "created snapshot %s of %s\n", name, rest[0])
	return err
}

func (c cli) snapshotRestore(args []string) error {
	fs := flag.NewFlagSet("snapshot restore", flag.ContinueOnError)
	fs.SetOutput(c.err)
	yes := fs.Bool("yes", false, "skip the running-cluster confirmation")
	force := fs.Bool("force", false, "restore even when the nodes it deletes hold the only copy of volume data")
	quiet := fs.Bool("quiet", false, "suppress stage narration")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return errors.New("usage: tbx snapshot restore <cluster> <name> [--yes] [--force] [--quiet]")
	}
	if err := c.ensureSnapshotRestoreSupport(); err != nil {
		return err
	}
	if err := c.confirmIfRunning(rest[0], *yes, "restore over"); err != nil {
		return err
	}
	var result daemon.SnapshotStatus
	if err := c.callNarrated("snapshot.restore", map[string]any{"cluster": rest[0], "name": rest[1], "force": *force}, &result, c.stages(*quiet)); err != nil {
		return err
	}
	if err := printWarnings(c.err, result.Warnings, result.Warning); err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.out, "restored %s from snapshot %s\n", rest[0], rest[1])
	return err
}

func (c cli) snapshotDelete(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: tbx snapshot delete <cluster> <name>")
	}
	var snaps []cluster.SnapshotInfo
	if err := c.call("snapshot.delete", map[string]string{"cluster": args[0], "name": args[1]}, &snaps); err != nil {
		return err
	}
	_, err := fmt.Fprintf(c.out, "deleted snapshot %s of %s\n", args[1], args[0])
	return err
}

func (c cli) snapshotList(args []string) error {
	flags := flag.NewFlagSet("snapshot list", flag.ContinueOnError)
	flags.SetOutput(c.err)
	outputFormat := flags.String("o", "table", "output format: table|json")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) > 1 {
		return errors.New("usage: tbx snapshot list [cluster] [-o json]")
	}
	if err := validateOutputFormat(*outputFormat); err != nil {
		return err
	}
	// No cluster lists every cluster's snapshots: after a destroy there is no
	// cluster name left to pass, and the residue check still has to be
	// answerable — with an empty result, not a usage error (#417).
	name := ""
	if len(positionals) == 1 {
		name = positionals[0]
	}
	var snaps []cluster.SnapshotInfo
	if err := c.call("snapshot.list", map[string]string{"cluster": name}, &snaps); err != nil {
		return err
	}
	if *outputFormat == "json" {
		if snaps == nil {
			snaps = []cluster.SnapshotInfo{}
		}
		return encodeJSON(c.out, snaps)
	}
	if len(snaps) == 0 {
		if name == "" {
			_, err := fmt.Fprintln(c.out, "no snapshots")
			return err
		}
		_, err := fmt.Fprintf(c.out, "no snapshots for %s\n", name)
		return err
	}
	table := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	if name == "" {
		// the cluster column only earns its place when the listing spans them
		if _, err := fmt.Fprintln(table, "CLUSTER\tNAME\tCREATED"); err != nil {
			return err
		}
		for _, snap := range snaps {
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s\n", snap.Cluster, snap.Name, snap.Created.Format("2006-01-02 15:04")); err != nil {
				return err
			}
		}
		return table.Flush()
	}
	if _, err := fmt.Fprintln(table, "NAME\tCREATED"); err != nil {
		return err
	}
	for _, snap := range snaps {
		if _, err := fmt.Fprintf(table, "%s\t%s\n", snap.Name, snap.Created.Format("2006-01-02 15:04")); err != nil {
			return err
		}
	}
	return table.Flush()
}

// confirmIfRunning prompts before an operation that will stop a running
// cluster's VMs, unless --yes was given. A non-interactive stdin declines.
func (c cli) confirmIfRunning(clusterName string, yes bool, action string) error {
	if yes {
		return nil
	}
	var clusters []daemon.ClusterStatus
	if err := c.call("status", map[string]string{"cluster": clusterName}, &clusters); err != nil {
		return err
	}
	if len(clusters) == 0 || !clusters[0].Running {
		return nil
	}
	_, _ = fmt.Fprintf(c.err, "cluster %s is running; %s will stop and restart it. Continue? [y/N] ", clusterName, action)
	answer, _ := bufio.NewReader(c.in).ReadString('\n')
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		return errors.New("aborted")
	}
	return nil
}

func defaultSnapshotName() string {
	return "snap-" + time.Now().Format("20060102-150405")
}
