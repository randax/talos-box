package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/provision"
	"github.com/randax/talos-box/internal/talosversion"
)

type upArgs struct {
	Talos    config.TalosSpec     `json:"talos"`
	Clusters []config.ClusterSpec `json:"clusters"`
	Force    bool                 `json:"force"`
}

// up reconciles the daemon's world toward the desired clusters: create the
// missing, start the stopped, leave the running alone.
func (s *Server) up(raw json.RawMessage) ([]Action, error) {
	return s.upWithMaintenance(raw, nil)
}

func (s *Server) upWithMaintenance(raw json.RawMessage, maintenance map[string]maintenanceObservation) ([]Action, error) {
	return s.upWithObservations(raw, maintenance, nil)
}

func (s *Server) upWithObservations(raw json.RawMessage, maintenance map[string]maintenanceObservation, storage map[string]storageObservation) ([]Action, error) {
	var args upArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	existing, err := s.existingStates()
	if err != nil {
		return nil, err
	}
	actions := PlanUp(args.Clusters, existing)
	if err := validateSpecVersions(args.Talos, args.Clusters, actions); err != nil {
		return nil, err
	}
	updates, err := s.preflightUpWithStorage(args.Clusters, existing, maintenance, storage)
	if err != nil {
		return nil, err
	}
	if err := persistIntentUpdates(updates); err != nil {
		return nil, err
	}
	for i, action := range actions {
		spec := args.Clusters[i]
		switch action.Kind {
		case ActionCreate:
			spec.Talos = resolveSpecTalos(spec, args.Talos)
			result, err := s.createFromSpec(spec, args.Force)
			if err != nil {
				return actions[:i], fmt.Errorf("create %s: %w", spec.Name, err)
			}
			actions[i].Warning = result.Warning
			actions[i].Warnings = result.Warnings
		case ActionStart:
			encoded, err := json.Marshal(startArgs{Name: spec.Name, Force: args.Force})
			if err != nil {
				return actions[:i], fmt.Errorf("encode start %s: %w", spec.Name, err)
			}
			result, err := s.startCluster(encoded)
			if err != nil {
				return actions[:i], fmt.Errorf("start %s: %w", spec.Name, err)
			}
			actions[i].Warning = result.Warning
			actions[i].Warnings = result.Warnings
		}
	}
	return actions, nil
}

func (s *Server) validateUp(raw json.RawMessage, maintenance map[string]maintenanceObservation, storage map[string]storageObservation) error {
	var args upArgs
	if err := decodeArgs(raw, &args); err != nil {
		return err
	}
	existing, err := s.existingStates()
	if err != nil {
		return err
	}
	if err := validateSpecVersions(args.Talos, args.Clusters, PlanUp(args.Clusters, existing)); err != nil {
		return err
	}
	_, err = s.preflightUpWithStorage(args.Clusters, existing, maintenance, storage)
	return err
}

// validateSpecVersions refuses an up request that would create a cluster
// with an effective version outside the support window, before any cluster
// is created or updated. Specs for existing clusters stay exempt: tbx echoes
// the created version into talosbox.yaml, so a floor bump must not stop
// `up` from starting what already exists.
func validateSpecVersions(fileTalos config.TalosSpec, specs []config.ClusterSpec, actions []Action) error {
	for i, spec := range specs {
		if actions[i].Kind != ActionCreate {
			continue
		}
		version := resolveSpecTalos(spec, fileTalos).Version
		if version == "" {
			continue
		}
		if err := talosversion.Validate(version); err != nil {
			return fmt.Errorf("cluster %s: %w", spec.Name, err)
		}
	}
	return nil
}

type maintenanceObservation struct {
	running map[string]bool
	phases  map[string]provision.Phase
}

type storageObservation struct {
	engine  cluster.CSI
	count   int
	running map[string]bool
}

func (observation storageObservation) matches(item cluster.Cluster, nodeRunning func(string) bool) bool {
	if observation.engine != item.CSI || len(observation.running) != len(item.Nodes) {
		return false
	}
	for _, node := range item.Nodes {
		wasRunning, ok := observation.running[node.Name]
		if !ok || wasRunning != nodeRunning(node.Name) {
			return false
		}
	}
	return true
}

func (s *Server) observeUpStorage(raw json.RawMessage) (map[string]storageObservation, error) {
	var args upArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	type candidate struct {
		item    cluster.Cluster
		running map[string]bool
	}
	candidates := make([]candidate, 0, len(args.Clusters))
	for _, spec := range args.Clusters {
		item, err := cluster.Load(spec.Name)
		if err != nil || item.CSI == "" || item.CSI == spec.CSI {
			continue
		}
		candidates = append(candidates, candidate{item: item})
	}
	s.opMu.Lock()
	for i := range candidates {
		candidates[i].running = make(map[string]bool, len(candidates[i].item.Nodes))
		for _, node := range candidates[i].item.Nodes {
			candidates[i].running[node.Name] = s.nodeRunning(candidates[i].item.Name, node.Name)
		}
	}
	s.opMu.Unlock()

	countVolumes := s.destroyVolumeCount
	if countVolumes == nil {
		countVolumes = countDestroyStorageVolumes
	}
	observations := make(map[string]storageObservation, len(candidates))
	for _, candidate := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		count, err := countVolumes(ctx, candidate.item)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("cluster %q: cannot verify %s volume count before changing csi: %w", candidate.item.Name, candidate.item.CSI, err)
		}
		observations[candidate.item.Name] = storageObservation{engine: candidate.item.CSI, count: count, running: candidate.running}
	}
	return observations, nil
}

func (s *Server) deleteUpStorageTransitions(raw json.RawMessage, observations map[string]storageObservation) error {
	if len(observations) == 0 {
		return nil
	}
	var args upArgs
	if err := decodeArgs(raw, &args); err != nil {
		return err
	}
	countVolumes := s.destroyVolumeCount
	if countVolumes == nil {
		countVolumes = countDestroyStorageVolumes
	}
	deleteEngine := s.storageEngineDelete
	if deleteEngine == nil {
		deleteEngine = deleteConfiguredStorageEngine
	}
	validateEngine := s.storageEngineValidate
	if validateEngine == nil {
		validateEngine = validateConfiguredStorageEngine
	}
	targets := make([]cluster.Cluster, 0, len(observations))
	for _, spec := range args.Clusters {
		observation, ok := observations[spec.Name]
		if !ok {
			continue
		}
		item, err := cluster.Load(spec.Name)
		if err != nil {
			return err
		}
		if item.CSI != observation.engine || item.CSI == spec.CSI {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		count, err := countVolumes(ctx, item)
		if err == nil && count > 0 {
			err = storageVolumesBlockChange(item, count)
		}
		if err == nil {
			err = validateEngine(ctx, item)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("cluster %q: validate removal of %s before changing csi: %w", item.Name, item.CSI, err)
		}
		targets = append(targets, item)
	}
	for _, item := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := deleteEngine(ctx, item)
		cancel()
		if err != nil {
			return fmt.Errorf("cluster %q: remove %s before changing csi: %w", item.Name, item.CSI, err)
		}
	}
	return nil
}

func deleteConfiguredStorageEngine(ctx context.Context, item cluster.Cluster) error {
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return err
	}
	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		return fmt.Errorf("read kubeconfig for storage cleanup: %w", err)
	}
	return provision.DeleteStorageEngineObjects(ctx, kubeconfig, item.CSI)
}

func validateConfiguredStorageEngine(ctx context.Context, item cluster.Cluster) error {
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		return err
	}
	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		return fmt.Errorf("read kubeconfig for storage cleanup validation: %w", err)
	}
	return provision.ValidateStorageEngineObjects(ctx, kubeconfig, item.CSI)
}

func (observation maintenanceObservation) sameSnapshot(other maintenanceObservation) bool {
	if len(observation.running) != len(other.running) || len(observation.phases) != len(other.phases) {
		return false
	}
	for name, running := range observation.running {
		if otherRunning, ok := other.running[name]; !ok || running != otherRunning {
			return false
		}
	}
	for name, phase := range observation.phases {
		if otherPhase, ok := other.phases[name]; !ok || phase != otherPhase {
			return false
		}
	}
	return true
}

func (observation maintenanceObservation) allNodesMaintenance(item cluster.Cluster, nodeRunning func(string) bool) bool {
	if len(item.Nodes) == 0 || len(observation.running) != len(item.Nodes) || len(observation.phases) != len(item.Nodes) {
		return false
	}
	for _, node := range item.Nodes {
		wasRunning, ok := observation.running[node.Name]
		if !ok || observation.phases[node.Name] != provision.PhaseMaintenance || wasRunning != nodeRunning(node.Name) {
			return false
		}
	}
	return true
}

func (s *Server) observeUpMaintenance(raw json.RawMessage) (map[string]maintenanceObservation, error) {
	var args upArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	type candidate struct {
		item    cluster.Cluster
		running map[string]bool
	}
	candidates := make([]candidate, 0, len(args.Clusters))
	load := s.maintenanceLoad
	if load == nil {
		load = cluster.Load
	}
	for _, spec := range args.Clusters {
		if spec.CNI == "" {
			continue
		}
		item, err := load(spec.Name)
		if err != nil {
			continue
		}
		if item.CNI != "" {
			continue
		}
		candidates = append(candidates, candidate{item: item})
	}
	s.opMu.Lock()
	for i := range candidates {
		candidates[i].running = make(map[string]bool, len(candidates[i].item.Nodes))
		for _, node := range candidates[i].item.Nodes {
			candidates[i].running[node.Name] = s.nodeRunning(candidates[i].item.Name, node.Name)
		}
	}
	s.opMu.Unlock()

	observations := make(map[string]maintenanceObservation, len(candidates))
	for _, candidate := range candidates {
		phases := make(map[string]provision.Phase, len(candidate.item.Nodes))
		for _, node := range s.observeProvisionNodesWithRunning(candidate.item, candidate.running) {
			phases[node.Name] = node.Phase
		}
		observations[candidate.item.Name] = maintenanceObservation{running: candidate.running, phases: phases}
	}
	return observations, nil
}

func actionAfterProvision(action ActionKind, narration []string) ActionKind {
	if action == ActionNone && len(narration) > 0 {
		return ActionReconcile
	}
	return action
}

type intentUpdate struct {
	next cluster.Cluster
}

func persistIntentUpdates(updates []intentUpdate) error {
	for _, update := range updates {
		if err := cluster.Save(update.next); err != nil {
			return fmt.Errorf("persist provisioning intent for %s: %w", update.next.Name, err)
		}
	}
	return nil
}

// preflightUp validates every existing cluster before it persists an intent
// update or starts a VM. That makes a multi-cluster `tbx up` fail atomically
// with respect to its declarative mutation rules: an invalid later cluster
// cannot leave an earlier one provisioned by surprise.
func (s *Server) preflightUp(
	specs []config.ClusterSpec,
	existing map[string]ClusterState,
	maintenance map[string]maintenanceObservation,
) ([]intentUpdate, error) {
	return s.preflightUpWithStorage(specs, existing, maintenance, nil)
}

func (s *Server) preflightUpWithStorage(
	specs []config.ClusterSpec,
	existing map[string]ClusterState,
	maintenance map[string]maintenanceObservation,
	storage map[string]storageObservation,
) ([]intentUpdate, error) {
	updates := make([]intentUpdate, 0, len(specs))
	for _, spec := range specs {
		if !existing[spec.Name].Exists {
			continue
		}
		item, err := cluster.Load(spec.Name)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", spec.Name, err)
		}
		if err := checkDomainUnchanged(item, spec); err != nil {
			return nil, err
		}
		allNodesMaintenance := false
		if item.CNI == "" && spec.CNI != "" {
			observation, ok := maintenance[item.Name]
			allNodesMaintenance = ok && observation.allNodesMaintenance(item, func(nodeName string) bool {
				return s.nodeRunning(item.Name, nodeName)
			})
		}
		var volumeCount *int
		if item.CSI != "" && item.CSI != spec.CSI {
			observation, ok := storage[item.Name]
			if !ok || !observation.matches(item, func(nodeName string) bool { return s.nodeRunning(item.Name, nodeName) }) {
				return nil, fmt.Errorf("cluster %q: storage state changed while verifying csi mutation; retry tbx up", item.Name)
			}
			volumeCount = &observation.count
		}
		intent, changed, err := reconcileProvisioningIntentWithVolumes(item, spec, allNodesMaintenance, volumeCount)
		if err != nil {
			return nil, err
		}
		if changed {
			item.ProvisioningIntent = intent
			updates = append(updates, intentUpdate{next: item})
		}
	}
	return updates, nil
}

// reconcileProvisioningIntent returns the only allowed persisted intent
// update for an existing cluster. It is deliberately pure: its caller first
// validates the complete desired set, then persists updates before running
// any host or Talos mutation.
func reconcileProvisioningIntent(item cluster.Cluster, spec config.ClusterSpec, allMaintenance bool) (cluster.ProvisioningIntent, bool, error) {
	return reconcileProvisioningIntentWithVolumes(item, spec, allMaintenance, nil)
}

func reconcileProvisioningIntentWithVolumes(item cluster.Cluster, spec config.ClusterSpec, allMaintenance bool, volumeCount *int) (cluster.ProvisioningIntent, bool, error) {
	current := item.ProvisioningIntent
	desired := spec.ProvisioningIntent
	if current.CNI == "" {
		if desired.CNI == "" {
			return current, false, nil
		}
		if !allMaintenance {
			return current, false, fmt.Errorf(
				"cluster %q: cni can be added only while all nodes are in maintenance mode; destroy and recreate after configuration",
				item.Name,
			)
		}
		return desired, true, nil
	}

	if desired.CNI != current.CNI {
		return current, false, fmt.Errorf(
			"cluster %q: cni is immutable once provisioning begins (cluster has %q, talosbox.yaml wants %q); run: tbx cluster destroy %s && tbx up",
			item.Name, current.CNI, desired.CNI, item.Name,
		)
	}
	if current.LB && !desired.LB {
		return current, false, fmt.Errorf(
			"cluster %q: lb is immutable once enabled; run: tbx cluster destroy %s && tbx up to disable it",
			item.Name, item.Name,
		)
	}

	next := current
	if !next.LB && desired.LB {
		next.LB = true
	}
	if desired.CSI != current.CSI {
		if current.CSI != "" {
			if volumeCount == nil {
				return current, false, fmt.Errorf("cluster %q: csi can be changed only after its volume count is verified", item.Name)
			}
			if *volumeCount > 0 {
				return current, false, storageVolumesBlockChange(item, *volumeCount)
			}
		}
		next.CSI = desired.CSI
	}
	// Hubble is the one symmetric optional desired set. CNI and LB rules above
	// have already fixed the cluster's irreversible substrate contract.
	if current.CNI == cluster.CNICilium {
		next.BGP = desired.BGP
		next.Hubble = desired.Hubble
	}
	return next, next != current, nil
}

func storageVolumesBlockChange(item cluster.Cluster, count int) error {
	return fmt.Errorf(
		"cluster %q: cannot change csi from %q while it has %d provisioned volume(s); delete the volumes first, or run: tbx cluster destroy %s",
		item.Name, item.CSI, count, item.Name,
	)
}

// down stops every cluster the file describes; it never destroys.
func (s *Server) down(raw json.RawMessage) ([]Action, error) {
	var args upArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	existing, err := s.existingStates()
	if err != nil {
		return nil, err
	}
	actions := PlanDown(args.Clusters, existing)
	for i, action := range actions {
		if action.Kind != ActionStop {
			continue
		}
		if err := s.stop(action.Cluster); err != nil {
			return actions[:i], fmt.Errorf("stop %s: %w", action.Cluster, err)
		}
	}
	return actions, nil
}

// checkDomainUnchanged rejects a talosbox.yaml that asks an existing cluster
// for a different domain: the domain is immutable (cert SANs bake it in), so
// silence here would misreport reality as reconciled.
func checkDomainUnchanged(item cluster.Cluster, spec config.ClusterSpec) error {
	if specEffectiveDomain(spec) != item.EffectiveDomain() {
		return fmt.Errorf(
			"cluster %q: domain is immutable (cluster has %q, talosbox.yaml wants %q); destroy and recreate the cluster to change it",
			spec.Name, item.EffectiveDomain(), specEffectiveDomain(spec),
		)
	}
	return nil
}

func specEffectiveDomain(spec config.ClusterSpec) string {
	if spec.Domain != "" {
		return spec.Domain
	}
	return spec.Name + "." + cluster.DefaultDomainSuffix
}

func (s *Server) existingStates() (map[string]ClusterState, error) {
	items, err := cluster.List()
	if err != nil {
		return nil, err
	}
	states := make(map[string]ClusterState, len(items))
	for _, item := range items {
		states[item.Name] = ClusterState{
			Exists:  true,
			Running: s.clusterRunning(item.Name),
			Ready: clusterReady(item, func(nodeName string) bool {
				return s.nodeRunning(item.Name, nodeName)
			}),
		}
	}
	return states, nil
}

func clusterReady(item cluster.Cluster, nodeActive func(string) bool) bool {
	if len(item.Nodes) == 0 {
		return false
	}
	for _, node := range item.Nodes {
		if !nodeActive(node.Name) {
			return false
		}
	}
	return true
}

// createFromSpec provisions and starts one cluster from a config spec.
func (s *Server) createFromSpec(spec config.ClusterSpec, force bool) (ClusterSummary, error) {
	args := createArgsFromSpec(spec, force)
	encoded, err := json.Marshal(args)
	if err != nil {
		return ClusterSummary{}, err
	}
	return s.createCluster(encoded)
}

// resolveSpecTalos returns the talos spec to create the cluster with. The
// client resolves per-cluster inheritance, so spec.Talos is authoritative; a
// fully zero spec comes from an older tbx that only ever sent the file-level
// block, which then still applies.
func resolveSpecTalos(spec config.ClusterSpec, fileTalos config.TalosSpec) config.TalosSpec {
	if spec.Talos.IsZero() {
		return fileTalos
	}
	return spec.Talos
}

func createArgsFromSpec(spec config.ClusterSpec, force bool) createArgs {
	return createArgs{
		Name:                    spec.Name,
		ControlPlanes:           &spec.ControlPlanes,
		Workers:                 &spec.Workers,
		Node:                    spec.Node,
		ControlPlane:            spec.ControlPlane,
		Worker:                  spec.Worker,
		ProvisioningIntentInput: spec.Input(),
		Domain:                  spec.Domain,
		AllowUnsafeDomain:       spec.AllowUnsafeDomain,
		Force:                   force,
		Schematic:               spec.Talos.Schematic,
		Version:                 spec.Talos.Version,
		Extensions:              spec.Talos.Extensions,
	}
}
