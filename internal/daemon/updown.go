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
		switch action.Kind {
		case ActionCreate:
			result, err := s.createFromSpec(spec, args.Talos, args.Force)
			if err != nil {
				return actions[:i], fmt.Errorf("create %s: %w", spec.Name, err)
			}
			actions[i].Warning = result.Warning
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
	args := createArgs{
		Name:          spec.Name,
		ControlPlanes: &spec.ControlPlanes,
		Workers:       &spec.Workers,
		Node:          spec.Node,
		ControlPlane:  spec.ControlPlane,
		Worker:        spec.Worker,
		BGP:           spec.BGP,
		Force:         force,
		Schematic:     talos.Schematic,
		Version:       talos.Version,
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return ClusterSummary{}, err
	}
	return s.createCluster(encoded)
}
