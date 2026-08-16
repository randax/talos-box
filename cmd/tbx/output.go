package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/randax/talos-box/internal/daemon"
)

func printClusters(output io.Writer, clusters []daemon.ClusterSummary) error {
	if len(clusters) == 0 {
		_, err := fmt.Fprintln(output, "No clusters.")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "NAME\tCONTROL PLANES\tWORKERS\tMEMORY\tCPUS\tDISK\tSTATE"); err != nil {
		return err
	}
	for _, item := range clusters {
		state := "stopped"
		if item.Running {
			state = "running"
		}
		if _, err := fmt.Fprintf(table, "%s\t%d\t%d\t%d MiB\t%d\t%d GiB\t%s\n",
			item.Name, item.ControlPlanes, item.Workers, item.NodeDefaults.MemoryMiB,
			item.NodeDefaults.CPUs, item.NodeDefaults.DiskGiB, state); err != nil {
			return err
		}
	}
	return table.Flush()
}

func printStatus(output io.Writer, clusters []daemon.ClusterStatus, quiet bool) error {
	if len(clusters) == 0 {
		_, err := fmt.Fprintln(output, "No clusters.")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "CLUSTER\tSUBNET\tDOMAIN\tTALOS\tNODE\tROLE\tMAC\tIP\tPHASE"); err != nil {
		return err
	}
	for _, item := range clusters {
		talos := item.TalosVersion
		if talos == "" {
			talos = "-"
		}
		for _, node := range item.Nodes {
			ip := node.IP
			if ip == "" {
				ip = "-"
			}
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				item.Name, item.Subnet, item.Domain, talos, node.Name, node.Role, node.MAC, ip, node.Phase); err != nil {
				return err
			}
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	for _, item := range clusters {
		if item.BGP {
			if _, err := fmt.Fprintf(output, "cluster %s: BGP mode enabled\n", item.Name); err != nil {
				return err
			}
		}
	}
	if quiet {
		return nil
	}
	// Every cluster carries a schematic (the daemon default when nothing was
	// pinned), so the full 64-hex ids stay out of quiet output.
	for _, item := range clusters {
		if item.Schematic != "" {
			if _, err := fmt.Fprintf(output, "cluster %s: schematic %s\n", item.Name, item.Schematic); err != nil {
				return err
			}
		}
		// A re-composed schematic boots from an id the user never wrote, so
		// the brought one is shown beside it.
		if item.BaseSchematic != "" {
			if _, err := fmt.Fprintf(output, "cluster %s: composed from schematic %s\n", item.Name, item.BaseSchematic); err != nil {
				return err
			}
		}
		// The composed id is opaque; the extension names it was composed
		// from are what the user wrote.
		if len(item.TalosExtensions) > 0 {
			if _, err := fmt.Fprintf(output, "cluster %s: extensions %s\n", item.Name, strings.Join(item.TalosExtensions, ", ")); err != nil {
				return err
			}
		}
	}
	printed := false
	for _, item := range clusters {
		for _, hint := range item.Hints {
			if !printed {
				if _, err := fmt.Fprintln(output); err != nil {
					return err
				}
				printed = true
			}
			if _, err := fmt.Fprintf(output, "hint [%s]: %s\n", item.Name, hint); err != nil {
				return err
			}
		}
	}
	return nil
}

// encodeJSON writes indented JSON — the machine-readable face of list/status.
func encodeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func cacheImageLine(entry daemon.CacheImageEntry) string {
	return fmt.Sprintf("%s %s %s %d bytes", entry.Schematic, entry.Version, entry.Architecture, entry.Size)
}

// cacheImageStatusSuffix names why a combination is kept. An older daemon
// reports no status at all, which prints as the pre-status line.
func cacheImageStatusSuffix(entry daemon.CacheImageEntry) string {
	switch {
	case entry.Status == "":
		return ""
	case entry.Status == daemon.CacheImageStatusInUse && len(entry.Clusters) > 0:
		return fmt.Sprintf(" in-use (%s)", strings.Join(entry.Clusters, ", "))
	default:
		return " " + string(entry.Status)
	}
}

// printPrunedImages names every combination a prune removes, with its size,
// ahead of the summary line: nothing about the default scope is automatic, so
// the user sees exactly what was unreferenced.
func printPrunedImages(output io.Writer, images []daemon.CacheImageEntry) error {
	return printCacheImageList(output, "Removing unreferenced disk images:", images)
}

// printWarmedImages summarises the mirror warming a file-aware pull performed.
// Only failures are named: a successful warm is one line, not one per image.
func printWarmedImages(output io.Writer, result *daemon.CacheWarmResult) error {
	if result == nil {
		return nil
	}
	for _, entry := range result.Entries {
		if entry.Status != daemon.CacheWarmStatusFailed {
			continue
		}
		if _, err := fmt.Fprintf(output, "✗ %s %s\n", entry.Ref, entry.Reason); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(output, "images: %d warmed, %d already complete, %d failed\n",
		result.Warmed, result.AlreadyComplete, result.Failed)
	return err
}

// printStrayImages names pinned combinations the pull found unclaimed. It is a
// report, not a plan: prune is the only thing that deletes.
func printStrayImages(output io.Writer, images []daemon.CacheImageEntry) error {
	return printCacheImageList(output, "Stray pinned disk images (no cluster, not in this file; nothing was removed):", images)
}

// printCacheImageList renders a headed list of combinations, or nothing at all
// when there are none: an empty section would read as a claim of its own.
func printCacheImageList(output io.Writer, header string, images []daemon.CacheImageEntry) error {
	if len(images) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(output, header); err != nil {
		return err
	}
	for _, entry := range images {
		if _, err := fmt.Fprintf(output, "- %s\n", cacheImageLine(entry)); err != nil {
			return err
		}
	}
	return nil
}
