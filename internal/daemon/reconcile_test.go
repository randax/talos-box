package daemon

import (
	"reflect"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
)

func TestPlanUp(t *testing.T) {
	desired := []config.ClusterSpec{
		{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"},
	}
	existing := map[string]ClusterState{
		"beta":  {Exists: true, Running: false},
		"gamma": {Exists: true, Running: true, Ready: true},
	}
	got := PlanUp(desired, existing)
	want := []Action{
		{Cluster: "alpha", Kind: ActionCreate},
		{Cluster: "beta", Kind: ActionStart},
		{Cluster: "gamma", Kind: ActionNone},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PlanUp = %+v, want %+v", got, want)
	}
}

func TestPlanUpStartsPartiallyRunningCluster(t *testing.T) {
	desired := []config.ClusterSpec{{Name: "partial"}}
	existing := map[string]ClusterState{
		"partial": {Exists: true, Running: true, Ready: false},
	}

	got := PlanUp(desired, existing)
	want := []Action{{Cluster: "partial", Kind: ActionStart}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PlanUp = %+v, want %+v", got, want)
	}
}

// Only the provisioning pass's fast no-op path — every desired outcome already
// observed healthy — leaves a planned no-op reported as "up to date". A pass
// that ran in full applied machine configs, bootstrapped etcd or re-applied the
// charts, and calling that "up to date" is the lie #358 set out to remove; its
// narration cannot tell the two apart, because a first bootstrap and a
// first-time chart install narrate exactly what a converged rerun does.
func TestActionAfterProvisionReservesUpToDateForTheFastNoop(t *testing.T) {
	tests := []struct {
		name     string
		action   ActionKind
		fullPass bool
		want     ActionKind
	}{
		{name: "running substrate-only cluster stays no-op", action: ActionNone, want: ActionNone},
		{name: "fast no-op provisioned cluster is up to date", action: ActionNone, want: ActionNone},
		{name: "full pass over a running cluster is reconciled", action: ActionNone, fullPass: true, want: ActionReconcile},
		{name: "stopped provisioned cluster starts before reconciliation", action: ActionStart, fullPass: true, want: ActionStart},
		{name: "created cluster keeps its own verb", action: ActionCreate, fullPass: true, want: ActionCreate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := actionAfterProvision(test.action, test.fullPass); got != test.want {
				t.Fatalf("actionAfterProvision(%q, %t) = %q, want %q", test.action, test.fullPass, got, test.want)
			}
		})
	}
}

func TestClusterReadyRequiresEveryPersistedNode(t *testing.T) {
	item := cluster.Cluster{Nodes: []cluster.Node{{Name: "cp-1"}, {Name: "worker-1"}}}
	active := map[string]bool{"cp-1": true, "worker-1": false}

	if clusterReady(item, func(name string) bool { return active[name] }) {
		t.Fatal("clusterReady() = true with an inactive persisted node")
	}
	active["worker-1"] = true
	if !clusterReady(item, func(name string) bool { return active[name] }) {
		t.Fatal("clusterReady() = false with every persisted node active")
	}
}

func TestPlanDown(t *testing.T) {
	desired := []config.ClusterSpec{
		{Name: "alpha"}, {Name: "beta"}, {Name: "ghost"},
	}
	existing := map[string]ClusterState{
		"alpha": {Exists: true, Running: true},
		"beta":  {Exists: true, Running: false},
	}
	got := PlanDown(desired, existing)
	want := []Action{
		{Cluster: "alpha", Kind: ActionStop},
		{Cluster: "beta", Kind: ActionNone},
		{Cluster: "ghost", Kind: ActionMissing},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PlanDown = %+v, want %+v", got, want)
	}
}
