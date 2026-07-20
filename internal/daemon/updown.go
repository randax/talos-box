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
			if err := s.createFromSpec(spec, args.Talos); err != nil {
				return actions[:i], fmt.Errorf("create %s: %w", spec.Name, err)
			}
		case ActionStart:
			item, err := cluster.Load(spec.Name)
			if err != nil {
				return actions[:i], err
			}
			if err := s.start(item); err != nil {
				return actions[:i], fmt.Errorf("start %s: %w", spec.Name, err)
			}
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
		states[item.Name] = ClusterState{Exists: true, Running: s.clusterRunning(item.Name)}
	}
	return states, nil
}

// createFromSpec provisions and starts one cluster from a config spec.
func (s *Server) createFromSpec(spec config.ClusterSpec, talos config.TalosSpec) error {
	args := createArgs{
		Name:          spec.Name,
		ControlPlanes: &spec.ControlPlanes,
		Workers:       &spec.Workers,
		Node:          spec.Node,
		ControlPlane:  spec.ControlPlane,
		Worker:        spec.Worker,
		BGP:           spec.BGP,
		Schematic:     talos.Schematic,
		Version:       talos.Version,
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	_, err = s.createCluster(encoded)
	return err
}
