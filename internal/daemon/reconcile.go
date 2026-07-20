package daemon

import "github.com/randax/talos-box/internal/config"

// ClusterState is what reconciliation knows about one existing cluster.
type ClusterState struct {
	Exists  bool
	Running bool
}

// ActionKind is what `tbx up`/`down` decided to do with one cluster.
type ActionKind string

const (
	ActionCreate ActionKind = "create"
	ActionStart  ActionKind = "start"
	ActionStop   ActionKind = "stop"
	ActionNone   ActionKind = "none"
	// ActionMissing marks a cluster the file describes but the host lacks.
	ActionMissing ActionKind = "missing"
)

// Action pairs a cluster with the reconciliation decision for it.
type Action struct {
	Cluster string     `json:"cluster"`
	Kind    ActionKind `json:"action"`
}

// PlanUp decides, per desired cluster: create it, start it, or leave it.
func PlanUp(desired []config.ClusterSpec, existing map[string]ClusterState) []Action {
	actions := make([]Action, 0, len(desired))
	for _, spec := range desired {
		state := existing[spec.Name]
		switch {
		case !state.Exists:
			actions = append(actions, Action{spec.Name, ActionCreate})
		case !state.Running:
			actions = append(actions, Action{spec.Name, ActionStart})
		default:
			actions = append(actions, Action{spec.Name, ActionNone})
		}
	}
	return actions
}

// PlanDown decides, per desired cluster: stop it if running, otherwise nothing.
func PlanDown(desired []config.ClusterSpec, existing map[string]ClusterState) []Action {
	actions := make([]Action, 0, len(desired))
	for _, spec := range desired {
		state := existing[spec.Name]
		switch {
		case !state.Exists:
			actions = append(actions, Action{spec.Name, ActionMissing})
		case state.Running:
			actions = append(actions, Action{spec.Name, ActionStop})
		default:
			actions = append(actions, Action{spec.Name, ActionNone})
		}
	}
	return actions
}
