package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/provision"
)

func (c cli) runManifests(args []string) error {
	flags := flag.NewFlagSet("manifests", flag.ContinueOnError)
	flags.SetOutput(c.err)
	cni := flags.String("cni", "", "curated cni to render CNI-derived sections for on a substrate-only cluster")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) < 1 || len(positionals) > 2 {
		return fmt.Errorf("usage: tbx manifests <cluster> [%s] [--cni cilium|flannel]", strings.Join(provision.InspectionSections(), "|"))
	}
	section := "all"
	if len(positionals) == 2 {
		section = positionals[1]
	}
	var clusters []daemon.ClusterSummary
	if err := c.call("cluster.list", struct{}{}, &clusters); err != nil {
		return err
	}
	for _, item := range clusters {
		if item.Name == positionals[0] {
			out, err := provision.RenderInspectionWithCNI(clusterFromSummary(item), section, *cni)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(c.out, out)
			return err
		}
	}
	return fmt.Errorf("cluster %q does not exist", positionals[0])
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
		Domain:             item.Domain,
		AllowUnsafeDomain:  item.AllowUnsafeDomain,
		Nodes:              nodes,
	}
}
