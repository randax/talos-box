package daemon

import (
	"encoding/json"
	"fmt"
	"log"

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

// setBGPLocked changes a live cluster's announcement mode end to end: the host
// speaker and the persisted intent first, then the forced Cilium reconcile that
// makes the cluster side match. Without that reconcile the mode change was
// host-only — Cilium kept running with bgpControlPlane disabled, its BGP CRDs
// absent, and the VIP still answering over L2, so the requested mechanism was
// never in effect while the verb reported success (#344).
//
// It must be called with opMu held: it registers the provisioning task the
// caller then runs, which is what lets a later mutation supersede it.
func (s *Server) setBGPLocked(raw json.RawMessage, enable bool, progress stageFunc) (*ClusterSummary, []provisionTask, error) {
	var args nameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, nil, err
	}
	if enable {
		progress.stage("starting the host BGP speaker for cluster %s", args.Name)
	} else {
		progress.stage("stopping the host BGP speaker for cluster %s", args.Name)
	}
	summary, err := s.setBGP(raw, enable)
	if err != nil {
		return nil, nil, err
	}
	item, err := cluster.Load(summary.Name)
	if err != nil {
		return nil, nil, err
	}
	tasks, deferred := s.beginBGPProvisionLocked(item)
	if deferred {
		summary.addWarnings(bgpDeferredReconcileWarning(item.Name, enable))
	}
	return &summary, tasks, nil
}

// beginBGPProvisionLocked schedules the reconcile a mode change needs. The task
// is forced for the same reason a topology change's is: the intent that decides
// what the Cilium chart renders just changed, and no health probe observes that
// — a converged L2 cluster looks complete right up to the moment BGP is asked
// for, so the fast no-op path would skip the re-render entirely.
//
// Like a topology change it can only converge over a fully-running cluster: the
// pass polls DHCP leases and readiness for every member. A partly-stopped
// cluster therefore defers it and reports that, rather than spinning to the
// provisioning timeout (#332).
func (s *Server) beginBGPProvisionLocked(item cluster.Cluster) ([]provisionTask, bool) {
	if !s.allNodesRunning(item) {
		log.Printf("bgp %s: cluster is only partly running, skipping reconcile", item.Name)
		return nil, clusterIsProvisioned(item) && len(item.Nodes) > 0
	}
	tasks := s.beginProvisionTasksLocked([]cluster.Cluster{item})
	for i := range tasks {
		tasks[i].force = true
	}
	return tasks, false
}

// bgpDeferredReconcileWarning explains the silence: the mode is recorded and the
// host speaker follows it, but Cilium still announces the old way until a
// reconcile runs over a fully-running cluster.
func bgpDeferredReconcileWarning(clusterName string, enable bool) string {
	requested, running := "bgp", "l2"
	if !enable {
		requested, running = "l2", "bgp"
	}
	return fmt.Sprintf(
		"cluster members are stopped; %s is recorded as %s but Cilium still announces over %s — start every member to reconcile it",
		clusterName, requested, running,
	)
}

// setBGP enables or disables host-side BGP for a cluster: it starts/stops the
// speaker in the helper and persists the mode. The Cilium side is reconciled by
// setBGPLocked's provisioning task, which renders the chart from the intent this
// saved.
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
