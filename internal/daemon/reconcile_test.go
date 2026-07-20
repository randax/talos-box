package daemon

import (
	"reflect"
	"testing"

	"github.com/randax/talos-box/internal/config"
)

func TestPlanUp(t *testing.T) {
	desired := []config.ClusterSpec{
		{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"},
	}
	existing := map[string]ClusterState{
		"beta":  {Exists: true, Running: false},
		"gamma": {Exists: true, Running: true},
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
