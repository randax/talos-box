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
	BGPStatus(cluster string) (helper.BGPState, error)
	Close() error
}

func hostBGPState(clusterName string) (helper.BGPState, error) {
	client, err := connectBGPHelper()
	if err != nil {
		return helper.BGPState{}, helperInstallError(err)
	}
	defer func() { _ = client.Close() }()
	state, err := client.BGPStatus(clusterName)
	if err != nil {
		return helper.BGPState{}, fmt.Errorf("read BGP speaker for %s: %w", clusterName, err)
	}
	return state, nil
}

func hostBGPActive(clusterName string) (bool, error) {
	state, err := hostBGPState(clusterName)
	return state.Active, err
}

// BGPRoute is one path the host speaker announces for a cluster.
type BGPRoute struct {
	Prefix  string `json:"prefix"`
	Nexthop string `json:"nexthop"`
}

// BGPStatus reports a cluster's announcement mode as it actually stands: the
// recorded intent, the host speaker behind it, and the routes that speaker has
// installed. `tbx bgp enable` used to be confirmable only through `doctor`,
// which is what left a refused enable indistinguishable from a half-done one
// (#399).
type BGPStatus struct {
	Name string `json:"name"`
	CNI  string `json:"cni,omitempty"`
	// BGP is the recorded announcement mode; Speaker is what the helper owns.
	BGP         bool       `json:"bgp"`
	Speaker     bool       `json:"speaker"`
	BindAddress string     `json:"bindAddress"`
	Port        int        `json:"port"`
	Routes      []BGPRoute `json:"routes,omitempty"`
	// SpeakerError names a helper that could not be asked, so a stopped
	// speaker is never reported from an unanswered question.
	SpeakerError string   `json:"speakerError,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

// bgpStatus answers `tbx bgp status <cluster>`: a read-only report, so it never
// touches the speaker or the cluster's intent.
func (s *Server) bgpStatus(raw json.RawMessage) (BGPStatus, error) {
	var args nameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return BGPStatus{}, err
	}
	item, err := cluster.Load(args.Name)
	if err != nil {
		return BGPStatus{}, err
	}
	status := BGPStatus{
		Name:        item.Name,
		CNI:         string(item.CNI),
		BGP:         item.BGP,
		BindAddress: cluster.Gateway(item.SubnetIndex),
		Port:        hostBGPPort,
	}
	state, err := hostBGPState(item.Name)
	if err != nil {
		status.SpeakerError = err.Error()
		return status, nil
	}
	status.Speaker = state.Active
	for _, route := range state.Routes {
		status.Routes = append(status.Routes, BGPRoute{Prefix: route.Prefix, Nexthop: route.Nexthop})
	}
	if state.Active {
		status.Warnings = appendNonEmpty(status.Warnings, bgpPortSquatterWarning(item))
	}
	return status, nil
}

func appendNonEmpty(list []string, value string) []string {
	if value == "" {
		return list
	}
	return append(list, value)
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
	// The pre-change state decides whether the cluster side has anything to
	// reconcile at all, and setBGP overwrites it.
	previous, err := cluster.Load(args.Name)
	if err != nil {
		return nil, nil, err
	}
	// The precondition is checked before anything is narrated: a refusal is a
	// pure validation failure, and narrating "starting the host BGP speaker"
	// ahead of it read as an action that failed halfway through when nothing
	// had been started at all. `cluster create --bgp` refuses with no narration
	// either (#399).
	if enable {
		if err := validateBGPIntent(previous); err != nil {
			return nil, nil, err
		}
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
	if !enable && !clusterSideBGPWasActive(previous) {
		// Disabling on a cluster whose Kubernetes side never announced over BGP
		// is a host-side withdrawal and nothing else: only Cilium renders BGP
		// objects at all, and a cluster that never had them has none to remove.
		// Forcing a full reconcile for that would re-render the whole stack —
		// and fail the verb on anything unrelated that is currently unhealthy.
		return &summary, nil, nil
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
	// Storage is deliberately out of scope: a BGP mode change re-renders the
	// CNI and its LoadBalancer objects, and nothing else. Dragging the storage
	// chart and its write/readback probe into the forced pass made `bgp enable`
	// fail on unrelated storage faults and hold the request for the storage
	// budget, while the memo storage already established stayed perfectly good.
	//
	// Except when the provision this one cancels was itself driving storage.
	// Registering this pass cancels the in-flight one
	// (beginProvisionTasksScopedLocked), and a scoped pass neither re-drives
	// storage nor invalidates the memo — so a `tbx up` cancelled mid
	// storage-install would never be resumed and the install would be stranded
	// with nothing scheduled to finish it. Taking the full scope resumes it.
	//
	// That is the only case worth the widening, because the full scope is not
	// free: this task is forced, and force bypasses tryFastNoopReconcile
	// entirely (provisionCNI), so the storage reconciler re-renders, re-applies
	// and re-probes unconditionally — on the request path, with the verb
	// failing on any unrelated storage fault. A cancelled pass that was itself
	// scoped (another BGP change) has no storage work to inherit, so this one
	// stays scoped too.
	//
	// A full-scope pass that then fails at the CNI stage leaves storage exactly
	// where any other pass that never reached the storage stage does: the memo
	// was invalidated at registration and the phase parked at `provisioning`,
	// and once the task retires, refreshStoragePhases starts a status probe
	// that can re-establish `live` on its own. Only a failure inside the
	// storage stage settles the terminal failed phase (#395), so a CNI-stage
	// failure never condemns storage that was live.
	active, provisionInFlight := s.provisions[item.Name]
	skipStorage := !provisionInFlight || active.skipStorage
	tasks := s.beginProvisionTasksScopedLocked([]cluster.Cluster{item}, skipStorage)
	for i := range tasks {
		tasks[i].force = true
	}
	return tasks, false
}

// clusterSideBGPWasActive reports whether the cluster's Kubernetes side was
// announcing over BGP before this change. Only Cilium renders BGP objects, and
// only while the recorded intent asked for them.
func clusterSideBGPWasActive(item cluster.Cluster) bool {
	return item.CNI == cluster.CNICilium && item.BGP
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
		if err := validateBGPIntent(item); err != nil {
			return ClusterSummary{}, err
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
	logHelperSyncFailure(fmt.Sprintf("sync helper state after the BGP change for %s", item.Name))
	result := summary(item, s.clusterRunning(item.Name))
	if enable {
		// The speaker is up; a foreign listener already holding the port is
		// something the operator has to know about, not a reason to fail the
		// verb that just took effect (#359).
		result.addWarnings(bgpPortSquatterWarning(item))
	}
	return result, nil
}

// validateBGPIntent reports whether the cluster's recorded intent can carry BGP
// at all. Only Cilium renders the announcement objects, so a substrate-only or
// flannel cluster is refused before any part of the change is attempted.
func validateBGPIntent(item cluster.Cluster) error {
	if item.CNI == "" {
		return fmt.Errorf("cluster %q cannot enable BGP: bgp requires cni: cilium", item.Name)
	}
	enabled := true
	var kubeletMemoryProtection *bool
	if item.DisableKubeletMemoryProtection {
		disabled := false
		kubeletMemoryProtection = &disabled
	}
	if _, err := cluster.ParseProvisioningIntent(string(item.CNI), string(item.CSI), &item.LB, &enabled, &item.Hubble, kubeletMemoryProtection); err != nil {
		return fmt.Errorf("cluster %q cannot enable BGP: %w", item.Name, err)
	}
	return nil
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
