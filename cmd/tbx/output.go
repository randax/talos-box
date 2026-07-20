package main

import (
	"encoding/json"
	"fmt"
	"io"
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
	if _, err := fmt.Fprintln(table, "CLUSTER\tSUBNET\tNODE\tROLE\tMAC\tIP\tPHASE"); err != nil {
		return err
	}
	for _, item := range clusters {
		for _, node := range item.Nodes {
			ip := node.IP
			if ip == "" {
				ip = "-"
			}
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				item.Name, item.Subnet, node.Name, node.Role, node.MAC, ip, node.Phase); err != nil {
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
