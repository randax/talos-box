package main

import (
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

func printStatus(output io.Writer, clusters []daemon.ClusterStatus) error {
	if len(clusters) == 0 {
		_, err := fmt.Fprintln(output, "No clusters.")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "CLUSTER\tNODE\tROLE\tMAC\tIP\tAPID\tVM"); err != nil {
		return err
	}
	for _, item := range clusters {
		vmState := "stopped"
		if item.Running {
			vmState = "running"
		}
		for _, node := range item.Nodes {
			ip := node.IP
			if ip == "" {
				ip = "-"
			}
			apid := "no"
			if node.APIDReachable {
				apid = "yes"
			}
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				item.Name, node.Name, node.Role, node.MAC, ip, apid, vmState); err != nil {
				return err
			}
		}
	}
	return table.Flush()
}
