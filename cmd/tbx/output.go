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
		switch {
		case item.Running:
			state = "running"
		case item.Suspended:
			state = "suspended"
		}
		if _, err := fmt.Fprintf(table, "%s\t%d\t%d\t%d MiB\t%d\t%d GiB\t%s\n",
			item.Name, item.ControlPlanes, item.Workers, item.NodeDefaults.MemoryMiB,
			item.NodeDefaults.CPUs, item.NodeDefaults.DiskGiB, state); err != nil {
			return err
		}
	}
	return table.Flush()
}

// nodePhase renders a node's phase, promoting a stopped node that holds its own
// saved memory to "suspended": it is on disk waiting to be resumed, and reading
// it as plain stopped is what led operators to start — the one verb that throws
// that memory away (#272). The flag is per node, not per cluster: suspend only
// saves the members that were running, and the ones that were already stopped
// stay honestly stopped.
func nodePhase(node daemon.NodeStatus) string {
	if node.Suspended && node.Phase == daemon.PhaseStopped {
		return "suspended"
	}
	return string(node.Phase)
}

// nodeKubelet renders a node's kubelet verdict, or a dash for a node the
// daemon could not ask — an unasked node is not a healthy one.
func nodeKubelet(node daemon.NodeStatus) string {
	if node.Kubelet == nil {
		return "-"
	}
	return string(node.Kubelet.Health)
}

// anyKubeletReading reports whether the printed set carries any kubelet
// reading at all, which is what decides the extra column.
func anyKubeletReading(clusters []daemon.ClusterStatus) bool {
	for _, item := range clusters {
		for _, node := range item.Nodes {
			if node.Kubelet != nil {
				return true
			}
		}
	}
	return false
}

func printStatus(output io.Writer, clusters []daemon.ClusterStatus, quiet bool) error {
	if len(clusters) == 0 {
		_, err := fmt.Fprintln(output, "No clusters.")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	header := "CLUSTER\tSUBNET\tDOMAIN\tTALOS\tNODE\tROLE\tMAC\tIP\tPHASE"
	// The kubelet column only appears once some node actually carries a
	// reading: a stopped cluster, a node in maintenance and an older daemon
	// report none, and a column of dashes would claim a measurement that was
	// never taken.
	services := anyKubeletReading(clusters)
	if services {
		header += "\tKUBELET"
	}
	if _, err := fmt.Fprintln(table, header); err != nil {
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
			row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
				item.Name, item.Subnet, item.Domain, talos, node.Name, node.Role, node.MAC, ip, nodePhase(node))
			if services {
				row += "\t" + nodeKubelet(node)
			}
			if _, err := fmt.Fprintln(table, row); err != nil {
				return err
			}
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	// A degraded kubelet is an observation, not advice, so it survives --quiet
	// alongside the other facts below: the column says which node, this line
	// says what Talos last reported about it (#357).
	for _, item := range clusters {
		for _, node := range item.Nodes {
			if node.Kubelet == nil || !node.Kubelet.Degraded() {
				continue
			}
			line := fmt.Sprintf("cluster %s: node %s kubelet %s", item.Name, node.Name, node.Kubelet.Health)
			if node.Kubelet.Message != "" {
				line += ": " + node.Kubelet.Message
			}
			if _, err := fmt.Fprintln(output, line); err != nil {
				return err
			}
		}
	}
	for _, item := range clusters {
		if item.BGP {
			if _, err := fmt.Fprintf(output, "cluster %s: BGP mode enabled\n", item.Name); err != nil {
				return err
			}
		}
	}
	// The schematic, what it was composed from, and the extensions that went
	// into it are facts about the cluster, not suggestions: --quiet drops hints
	// only, so these survive it (#307).
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
	if quiet {
		return nil
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

// cacheImageLine names a cached combination with both sizes: a disk.raw is
// sparse, so the apparent size alone overstates what the cache costs. An older
// daemon reports no allocated size, which prints as the pre-allocated line.
func cacheImageLine(entry daemon.CacheImageEntry) string {
	line := fmt.Sprintf("%s %s %s %d bytes", entry.Schematic, entry.Version, entry.Architecture, entry.Size)
	if entry.AllocatedSize > 0 {
		line += fmt.Sprintf(" (%d bytes on disk)", entry.AllocatedSize)
	}
	if entry.Incomplete {
		// Leftovers with no usable image: listed so an unscoped prune holds
		// no surprises, marked so they are not mistaken for a warm image.
		line += " (incomplete)"
	}
	return line
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

// validateOutputFormat rejects -o values none of the table/json printers
// understand, so a typo errors instead of silently printing the table.
func validateOutputFormat(format string) error {
	if format != "table" && format != "json" {
		return fmt.Errorf("unknown output format %q: want table or json", format)
	}
	return nil
}
