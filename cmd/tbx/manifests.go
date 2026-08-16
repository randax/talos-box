package main

import (
	"fmt"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/provision"
)

func (c cli) runManifests(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: tbx manifests <cluster> [%s]", strings.Join(provision.InspectionSections(), "|"))
	}
	section := "all"
	if len(args) == 2 {
		section = args[1]
	}
	var clusters []daemon.ClusterSummary
	if err := c.call("cluster.list", struct{}{}, &clusters); err != nil {
		return err
	}
	for _, item := range clusters {
		if item.Name == args[0] {
			out, err := provision.RenderInspection(clusterFromSummary(item), section)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(c.out, out)
			return err
		}
	}
	return fmt.Errorf("cluster %q does not exist", args[0])
}

func clusterFromSummary(item daemon.ClusterSummary) cluster.Cluster {
	nodes := make([]cluster.Node, 0, item.ControlPlanes+item.Workers)
	for range item.ControlPlanes {
		nodes = append(nodes, cluster.Node{Role: cluster.RoleControlPlane})
	}
	for range item.Workers {
		nodes = append(nodes, cluster.Node{Role: cluster.RoleWorker})
	}
	return cluster.Cluster{
		Name:        item.Name,
		SubnetIndex: item.SubnetIndex,
		// The pin travels with the summary because the images section
		// names the Talos system images the cluster was created against.
		Schematic:          item.Schematic,
		TalosVersion:       item.TalosVersion,
		ProvisioningIntent: item.ProvisioningIntent,
		Nodes:              nodes,
	}
}
