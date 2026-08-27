package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/provision"
	"github.com/randax/talos-box/internal/shellquote"
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
	return s.upWithObservations(raw, maintenance, nil, nil)
}

// upWithObservations runs the pass. progress narrates the per-cluster creates:
// they run under the operation lock and a cold image fetch is minutes of work,
// so a silent pass would leave the client's liveness bound with nothing to
// re-arm it (#392). Each line is prefixed with the cluster it belongs to,
// because one pass speaks for several.
func (s *Server) upWithObservations(raw json.RawMessage, maintenance map[string]maintenanceObservation, storage map[string]storageObservation, progress stageFunc) ([]Action, error) {
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
			result, err := s.createFromSpec(spec, args.Force, clusterStages(progress, spec.Name))
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
	engine cluster.CSI
	// volumes names the claims behind the engine's PersistentVolumes, so a
	// refusal can list what blocks the switch (#393). Its length is the count
	// the gate turns on.
	volumes []string
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

	listVolumes := s.storageVolumeClaims
	if listVolumes == nil {
		listVolumes = listStorageVolumeClaims
	}
	observations := make(map[string]storageObservation, len(candidates))
	for _, candidate := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		volumes, err := listVolumes(ctx, candidate.item)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("cluster %q: cannot verify %s volume count before changing csi: %w", candidate.item.Name, candidate.item.CSI, err)
		}
		observations[candidate.item.Name] = storageObservation{engine: candidate.item.CSI, volumes: volumes, running: candidate.running}
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
	listVolumes := s.storageVolumeClaims
	if listVolumes == nil {
		listVolumes = listStorageVolumeClaims
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
		volumes, err := listVolumes(ctx, item)
		if err == nil && len(volumes) > 0 {
			err = storageVolumesBlockChange(item, volumes)
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

// actionAfterProvision promotes a planned no-op to a reconcile whenever the
// provisioning pass ran in full. The pass itself is the only witness of that:
// its fast no-op path fires exactly when every desired outcome is already
// observed healthy, and a full pass applies machine configs, bootstraps etcd
// and re-applies the charts — work `tbx up` must not report as "up to date"
// (#358). Narration cannot stand in for it: a first etcd bootstrap and a
// first-time MetalLB install narrate the same lines a converged rerun does.
func actionAfterProvision(action ActionKind, fullPass bool) ActionKind {
	if action == ActionNone && fullPass {
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
	if len(updates) != 0 {
		logHelperSyncFailure("sync helper state after persisting provisioning intent")
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
		var volumes *[]string
		if item.CSI != "" && item.CSI != spec.CSI {
			observation, ok := storage[item.Name]
			if !ok || !observation.matches(item, func(nodeName string) bool { return s.nodeRunning(item.Name, nodeName) }) {
				return nil, fmt.Errorf("cluster %q: storage state changed while verifying csi mutation; retry tbx up", item.Name)
			}
			volumes = &observation.volumes
		}
		intent, changed, err := reconcileProvisioningIntentWithVolumes(item, spec, allNodesMaintenance, volumes)
		if err != nil {
			return nil, err
		}
		// An existing cluster named by talosbox.yaml is config-managed from
		// this up onwards, whether it was created imperatively or by a tbx
		// predating the flag: the file can rerun it, so its hints may say so
		// (#267).
		claimed := item.ConfigOrigin != cluster.OriginManaged
		item.ConfigOrigin = cluster.OriginManaged
		if changed || claimed {
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

func reconcileProvisioningIntentWithVolumes(item cluster.Cluster, spec config.ClusterSpec, allMaintenance bool, volumes *[]string) (cluster.ProvisioningIntent, bool, error) {
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
			item.Name, current.CNI, desired.CNI, shellquote.Quote(item.Name),
		)
	}
	if current.LB && !desired.LB {
		return current, false, fmt.Errorf(
			"cluster %q: lb is immutable once enabled; run: tbx cluster destroy %s && tbx up to disable it",
			item.Name, shellquote.Quote(item.Name),
		)
	}

	next := current
	if !next.LB && desired.LB {
		next.LB = true
	}
	if desired.CSI != current.CSI {
		if current.CSI != "" {
			if volumes == nil {
				return current, false, fmt.Errorf("cluster %q: csi can be changed only after its volume count is verified", item.Name)
			}
			if len(*volumes) > 0 {
				return current, false, storageVolumesBlockChange(item, *volumes)
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

// storageVolumesBlockChange refuses a csi switch by naming what blocks it. A
// count alone leaves the operator to go and find the volumes themselves, which
// is the work the refusal exists to save them (#393); the inflection follows
// the destroy warning's.
func storageVolumesBlockChange(item cluster.Cluster, volumes []string) error {
	return fmt.Errorf(
		"cluster %q: cannot change csi from %q while it has %d provisioned %s (%s); delete the volumes first, or run: tbx cluster destroy %s",
		item.Name, item.CSI, len(volumes), Unit(len(volumes), "volume", "volumes"), storageVolumeList(volumes), shellquote.Quote(item.Name),
	)
}

// storageVolumeListCap is how many volumes a refusal names before it stops:
// enough to act on, few enough that the refusal stays a line an operator reads
// rather than a dump they scroll past.
const storageVolumeListCap = 5

func storageVolumeList(volumes []string) string {
	if len(volumes) <= storageVolumeListCap {
		return strings.Join(volumes, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(volumes[:storageVolumeListCap], ", "), len(volumes)-storageVolumeListCap)
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
			Ready:   s.allNodesRunning(item),
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
func (s *Server) createFromSpec(spec config.ClusterSpec, force bool, progress stageFunc) (ClusterSummary, error) {
	args := createArgsFromSpec(spec, force)
	encoded, err := json.Marshal(args)
	if err != nil {
		return ClusterSummary{}, err
	}
	return s.createCluster(encoded, progress)
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
		ConfigManaged:           true,
	}
}
