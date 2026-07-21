package balloon

import (
	"reflect"
	"testing"
)

func TestNoDeficitDeflatesToConfigured(t *testing.T) {
	nodes := []Node{
		{Name: "a", ConfiguredMiB: 2048},
		{Name: "b", ConfiguredMiB: 4096},
	}
	got := PlanTargets(nodes, 0, 1024)
	want := map[string]int{"a": 2048, "b": 4096}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("no deficit: targets = %v, want configured %v", got, want)
	}
}

func TestProportionalInflation(t *testing.T) {
	nodes := []Node{
		{Name: "a", ConfiguredMiB: 2048},
		{Name: "b", ConfiguredMiB: 2048},
	}
	// reclaim 1024 MiB across two equal nodes -> 512 each
	got := PlanTargets(nodes, 1024, 1024)
	want := map[string]int{"a": 1536, "b": 1536}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

func TestProportionalBySize(t *testing.T) {
	nodes := []Node{
		{Name: "small", ConfiguredMiB: 1024},
		{Name: "big", ConfiguredMiB: 3072},
	}
	// reclaim 1024 across 4096 total: small gives 256, big gives 768
	got := PlanTargets(nodes, 1024, 512)
	want := map[string]int{"small": 768, "big": 2304}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

func TestFloorRedistributesDeficit(t *testing.T) {
	nodes := []Node{
		{Name: "small", ConfiguredMiB: 2048},
		{Name: "big", ConfiguredMiB: 8192},
	}
	// reclaim 3072 with floor 2048. Proportional would push small to
	// 2048 - 3072*(2048/10240)=2048-614=1434 < floor 2048, so small clamps to
	// 2048 (gives 0) and big must absorb all 3072 -> 8192-3072=5120.
	got := PlanTargets(nodes, 3072, 2048)
	want := map[string]int{"small": 2048, "big": 5120}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

func TestDeficitExceedingReclaimableClampsAllToFloor(t *testing.T) {
	nodes := []Node{
		{Name: "a", ConfiguredMiB: 2048},
		{Name: "b", ConfiguredMiB: 2048},
	}
	// reclaimable = 2*(2048-1024)=2048; ask for 9999 -> everyone at floor
	got := PlanTargets(nodes, 9999, 1024)
	want := map[string]int{"a": 1024, "b": 1024}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %v, want %v (all at floor)", got, want)
	}
}

func TestOvercommitWarnsBeyondReserve(t *testing.T) {
	// host 16 GiB, reserve 6 GiB -> 10 GiB available for VMs
	tests := []struct {
		plannedMiB int
		want       bool
	}{
		{8192, false},  // 8 < 10, fine
		{10240, false}, // exactly 10, fine
		{10241, true},  // over
		{14000, true},
	}
	for _, tt := range tests {
		if got := Overcommit(tt.plannedMiB, 16384, 6144); got != tt.want {
			t.Errorf("Overcommit(%d) = %v, want %v", tt.plannedMiB, got, tt.want)
		}
	}
}
