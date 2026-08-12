package daemon

import (
	"encoding/json"
	"fmt"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/manifests"
)

type bgpHelperClient interface {
	EnableBGP(cluster string, subnetIndex int, localASN, peerASN uint32) error
	DisableBGP(cluster string) error
	HasBGP(cluster string) (bool, error)
	Close() error
}

func hostBGPActive(clusterName string) (bool, error) {
	client, err := connectBGPHelper()
	if err != nil {
		return false, helperInstallError(err)
	}
	defer func() { _ = client.Close() }()
	active, err := client.HasBGP(clusterName)
	if err != nil {
		return false, fmt.Errorf("read BGP speaker for %s: %w", clusterName, err)
	}
	return active, nil
}

var connectBGPHelper = func() (bgpHelperClient, error) { return helper.Connect() }

// setBGP enables or disables host-side BGP for a cluster: it starts/stops the
// speaker in the helper and persists the mode. The attendee still applies the
// Cilium BGP resources from `tbx manifests` — this brings up the host peer.
func (s *Server) setBGP(raw json.RawMessage, enable bool) (ClusterSummary, error) {
	var args nameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}
	if enable {
		if item.CNI == "" {
			return ClusterSummary{}, fmt.Errorf("cluster %q cannot enable BGP: bgp requires cni: cilium", item.Name)
		}
		enabled := true
		if _, err := cluster.ParseProvisioningIntent(string(item.CNI), string(item.CSI), &item.LB, &enabled, &item.Hubble); err != nil {
			return ClusterSummary{}, fmt.Errorf("cluster %q cannot enable BGP: %w", item.Name, err)
		}
	}

	if enable {
		if err := enableHostBGP(item); err != nil {
			return ClusterSummary{}, err
		}
	} else if err := disableHostBGP(item.Name); err != nil {
		return ClusterSummary{}, err
	}

	item.BGP = enable
	if err := cluster.Save(item); err != nil {
		return ClusterSummary{}, err
	}
	return summary(item, s.clusterRunning(item.Name)), nil
}

func enableHostBGP(item cluster.Cluster) error {
	client, err := connectBGPHelper()
	if err != nil {
		return helperInstallError(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.EnableBGP(item.Name, item.SubnetIndex, uint32(manifests.HostASN), uint32(manifests.ClusterASN(item.SubnetIndex))); err != nil {
		return fmt.Errorf("enable BGP speaker for %s: %w", item.Name, err)
	}
	return nil
}

func disableHostBGP(clusterName string) error {
	client, err := connectBGPHelper()
	if err != nil {
		return helperInstallError(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.DisableBGP(clusterName); err != nil {
		return fmt.Errorf("disable BGP speaker for %s: %w", clusterName, err)
	}
	return nil
}
