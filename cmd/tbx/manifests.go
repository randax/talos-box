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
	return cluster.Cluster{
		Name:               item.Name,
		SubnetIndex:        item.SubnetIndex,
		ProvisioningIntent: item.ProvisioningIntent,
	}
}
