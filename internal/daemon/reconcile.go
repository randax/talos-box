package daemon

import "github.com/randax/talos-box/internal/config"

// ClusterState is what reconciliation knows about one existing cluster.
type ClusterState struct {
	Exists  bool
	Running bool
	Ready   bool
}

// ActionKind is what `tbx up`/`down` decided to do with one cluster.
type ActionKind string

const (
	ActionCreate ActionKind = "create"
	ActionStart  ActionKind = "start"
	// ActionReconcile marks a running provisioned cluster whose Talos/Kubernetes
	// end state is still incomplete. It is intentionally distinct from a VM
	// no-op so the CLI never calls a half-provisioned cluster "up to date".
	ActionReconcile ActionKind = "reconcile"
	ActionStop      ActionKind = "stop"
	ActionNone      ActionKind = "none"
	// ActionMissing marks a cluster the file describes but the host lacks.
	ActionMissing ActionKind = "missing"
)

// Action pairs a cluster with the reconciliation decision for it.
type Action struct {
	Cluster string     `json:"cluster"`
	Kind    ActionKind `json:"action"`
	Warning string     `json:"warning,omitempty"`
	// Warnings is the same set as Warning, one entry per finding, so the CLI
	// can render them one per line. Warning stays populated for old clients.
	Warnings  []string `json:"warnings,omitempty"`
	Narration []string `json:"narration,omitempty"`
}

// PlanUp decides, per desired cluster: create it, start it, or leave it.
func PlanUp(desired []config.ClusterSpec, existing map[string]ClusterState) []Action {
	actions := make([]Action, 0, len(desired))
	for _, spec := range desired {
		state := existing[spec.Name]
		switch {
		case !state.Exists:
			actions = append(actions, Action{Cluster: spec.Name, Kind: ActionCreate})
		case !state.Ready:
			actions = append(actions, Action{Cluster: spec.Name, Kind: ActionStart})
		default:
			actions = append(actions, Action{Cluster: spec.Name, Kind: ActionNone})
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
			actions = append(actions, Action{Cluster: spec.Name, Kind: ActionMissing})
		case state.Running:
			actions = append(actions, Action{Cluster: spec.Name, Kind: ActionStop})
		default:
			actions = append(actions, Action{Cluster: spec.Name, Kind: ActionNone})
		}
	}
	return actions
}
