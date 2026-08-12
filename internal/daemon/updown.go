package daemon

import (
	"encoding/json"
	"fmt"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
)

type upArgs struct {
	Talos    config.TalosSpec     `json:"talos"`
	Clusters []config.ClusterSpec `json:"clusters"`
	Force    bool                 `json:"force"`
}

// up reconciles the daemon's world toward the desired clusters: create the
// missing, start the stopped, leave the running alone.
func (s *Server) up(raw json.RawMessage) ([]Action, error) {
	var args upArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	existing, err := s.existingStates()
	if err != nil {
		return nil, err
	}
	actions := PlanUp(args.Clusters, existing)
	for i, action := range actions {
		spec := args.Clusters[i]
		if action.Kind == ActionStart || action.Kind == ActionNone {
			if err := checkDomainUnchanged(spec); err != nil {
				return actions[:i], err
			}
		}
		switch action.Kind {
		case ActionCreate:
			result, err := s.createFromSpec(spec, args.Talos, args.Force)
			if err != nil {
				return actions[:i], fmt.Errorf("create %s: %w", spec.Name, err)
			}
			actions[i].Warning = result.Warning
			actions[i].Narration = result.Narration
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
			actions[i].Narration = result.Narration
		case ActionNone:
			item, err := cluster.Load(spec.Name)
			if err != nil {
				return actions[:i], fmt.Errorf("load %s: %w", spec.Name, err)
			}
			narration, err := s.provisionFlannel(item)
			if err != nil {
				return actions[:i], fmt.Errorf("provision %s: %w", spec.Name, err)
			}
			actions[i].Narration = narration
		}
	}
	return actions, nil
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
func checkDomainUnchanged(spec config.ClusterSpec) error {
	item, err := cluster.Load(spec.Name)
	if err != nil {
		return nil // partially-created state; create/start surfaces the real error
	}
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
func (s *Server) createFromSpec(spec config.ClusterSpec, talos config.TalosSpec, force bool) (ClusterSummary, error) {
	args := createArgsFromSpec(spec, talos, force)
	encoded, err := json.Marshal(args)
	if err != nil {
		return ClusterSummary{}, err
	}
	return s.createCluster(encoded)
}

func createArgsFromSpec(spec config.ClusterSpec, talos config.TalosSpec, force bool) createArgs {
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
		Schematic:               talos.Schematic,
		Version:                 talos.Version,
	}
}
