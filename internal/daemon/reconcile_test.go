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

func TestActionAfterProvisionDoesNotCallReconciledClusterUpToDate(t *testing.T) {
	tests := []struct {
		name      string
		action    ActionKind
		narration []string
		want      ActionKind
	}{
		{name: "running substrate-only cluster stays no-op", action: ActionNone, want: ActionNone},
		{name: "fast no-op provisioned cluster is up to date", action: ActionNone, want: ActionNone},
		{name: "provisioning work is reported as reconciled", action: ActionNone, narration: []string{"≈ kubectl apply --server-side -f -"}, want: ActionReconcile},
		{name: "stopped provisioned cluster starts before reconciliation", action: ActionStart, narration: []string{"≈ kubectl apply --server-side -f -"}, want: ActionStart},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := actionAfterProvision(test.action, test.narration); got != test.want {
				t.Fatalf("actionAfterProvision(%q, %q) = %q, want %q", test.action, test.narration, got, test.want)
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
