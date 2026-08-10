//go:build linux

package helper

import (
	"encoding/json"
	"fmt"

	"github.com/randax/talos-box/internal/bgp"
)

type bgpArgs struct {
	Cluster     string `json:"cluster"`
	SubnetIndex *int   `json:"subnetIndex"`
	LocalASN    uint32 `json:"localASN"`
	PeerASN     uint32 `json:"peerASN"`
}

type linuxBGPSpeakerStarter func(localASN, peerASN uint32, gatewayIP, peerCIDR string, fib bgp.FIB) (bgpSpeaker, error)

var startLinuxBGPSpeaker linuxBGPSpeakerStarter = func(localASN, peerASN uint32, gatewayIP, peerCIDR string, fib bgp.FIB) (bgpSpeaker, error) {
	return bgp.StartSpeaker(localASN, peerASN, gatewayIP, peerCIDR, fib)
}

func (s *Server) enableBGP(raw json.RawMessage) error {
	var args bgpArgs
	if err := decodeArgs(raw, &args); err != nil {
		return err
	}
	if err := validateBGPEnableArgs(args.Cluster, args.SubnetIndex, args.LocalASN, args.PeerASN); err != nil {
		return err
	}
	if s.speakers == nil {
		s.speakers = map[string]bgpSpeaker{}
	}
	if _, ok := s.speakers[args.Cluster]; ok {
		return nil
	}
	gateway := fmt.Sprintf("172.30.%d.1", *args.SubnetIndex)
	peerCIDR := fmt.Sprintf("172.30.%d.0/24", *args.SubnetIndex)
	speaker, err := startLinuxBGPSpeaker(args.LocalASN, args.PeerASN, gateway, peerCIDR, routeFIB{})
	if err != nil {
		return fmt.Errorf("start bgp speaker for %s: %w", args.Cluster, err)
	}
	s.speakers[args.Cluster] = speaker
	return nil
}

func (s *Server) disableBGP(raw json.RawMessage) error {
	var args bgpArgs
	if err := decodeArgs(raw, &args); err != nil {
		return err
	}
	if err := validateBGPCluster(args.Cluster); err != nil {
		return err
	}
	speaker, ok := s.speakers[args.Cluster]
	if !ok {
		return nil
	}
	speaker.Stop()
	delete(s.speakers, args.Cluster)
	return nil
}
