package daemon

import (
	"encoding/json"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/manifests"
)

// setBGP enables or disables host-side BGP for a cluster: it starts/stops the
// speaker in the helper and persists the mode. The attendee still applies the
// CiliumBGPPeeringPolicy from `tbx manifests` — this brings up the host peer.
func (s *Server) setBGP(raw json.RawMessage, enable bool) (ClusterSummary, error) {
	var args nameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return ClusterSummary{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return ClusterSummary{}, err
	}

	client, err := helper.Connect()
	if err != nil {
		return ClusterSummary{}, helperInstallError(err)
	}
	defer func() { _ = client.Close() }()

	if enable {
		localASN := uint32(manifests.HostASN)
		peerASN := uint32(manifests.ClusterASN(item.SubnetIndex))
		if err := client.EnableBGP(item.Name, item.SubnetIndex, localASN, peerASN); err != nil {
			return ClusterSummary{}, err
		}
	} else if err := client.DisableBGP(item.Name); err != nil {
		return ClusterSummary{}, err
	}

	item.BGP = enable
	if err := cluster.Save(item); err != nil {
		return ClusterSummary{}, err
	}
	return summary(item, s.clusterRunning(item.Name)), nil
}
