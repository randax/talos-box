package daemon

import (
	"strings"

	"github.com/randax/talos-box/internal/config"
)

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
	// ActionReconcile marks a running provisioned cluster whose provisioning
	// pass ran in full — a node still in maintenance, a drifted machine config,
	// an etcd that had never been bootstrapped, a chart the cluster did not yet
	// have. It is intentionally distinct from a VM no-op so the CLI never calls
	// a half-provisioned cluster "up to date"; ActionNone is reserved for the
	// pass's fast no-op path, which fires only once every desired outcome is
	// observed healthy (#358).
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

// addWarnings records findings the pass only learned after the action was
// decided — the out-of-lock boot wait — behind the ones already there, and
// keeps the legacy joined string in step for an old CLI.
func (a *Action) addWarnings(warnings ...string) {
	existing := a.Warnings
	if len(existing) == 0 && a.Warning != "" {
		// An action that carries only the legacy joined string keeps it: this
		// must add findings, never drop one.
		existing = []string{a.Warning}
	}
	a.Warnings = warningList(append(append([]string{}, existing...), warnings...)...)
	a.Warning = strings.Join(a.Warnings, "; ")
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
