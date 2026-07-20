package cluster

import (
	"net"
	"reflect"
	"testing"
)

func TestDeterministicMAC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cluster string
		node    string
		want    string
	}{
		{name: "control plane", cluster: "demo", node: "demo-cp-1", want: "52:54:00:94:25:87"},
		{name: "worker", cluster: "demo", node: "demo-worker-1", want: "52:54:00:39:a2:1b"},
		{name: "cluster contributes", cluster: "other", node: "demo-worker-1", want: "52:54:00:4c:c1:b6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := DeterministicMAC(tt.cluster, tt.node)
			if got != tt.want {
				t.Fatalf("DeterministicMAC(%q, %q) = %q, want %q", tt.cluster, tt.node, got, tt.want)
			}
			mac, err := net.ParseMAC(got)
			if err != nil {
				t.Fatal(err)
			}
			if mac[0]&1 != 0 || mac[0]&2 == 0 {
				t.Fatalf("MAC %q is not unicast and locally administered", got)
			}
		})
	}
}

func TestStateRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tests := []struct {
		name     string
		index    int
		cp       int
		workers  int
		defaults NodeDefaults
	}{
		{name: "defaults", index: 3, cp: 1, workers: 2},
		{name: "custom", index: 4, cp: 3, workers: 1, defaults: NodeDefaults{MemoryMiB: 4096, CPUs: 4, DiskGiB: 40}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, err := New(tt.name, tt.index, tt.cp, tt.workers, tt.defaults)
			if err != nil {
				t.Fatal(err)
			}
			if err := Save(want); err != nil {
				t.Fatal(err)
			}
			got, err := Load(want.Name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Load(%q) = %#v, want %#v", want.Name, got, want)
			}
		})
	}
}

func TestLowestFreeSubnetIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		clusters []Cluster
		want     int
	}{
		{name: "empty", want: 0},
		{name: "fills first gap", clusters: []Cluster{{SubnetIndex: 0}, {SubnetIndex: 2}}, want: 1},
		{name: "ignores order", clusters: []Cluster{{SubnetIndex: 3}, {SubnetIndex: 1}, {SubnetIndex: 0}}, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := LowestFreeSubnetIndex(tt.clusters)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("LowestFreeSubnetIndex() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLowestFreeSubnetIndexExhausted(t *testing.T) {
	t.Parallel()
	clusters := make([]Cluster, MaxSubnetIndex+1)
	for index := range clusters {
		clusters[index].SubnetIndex = index
	}
	if _, err := LowestFreeSubnetIndex(clusters); err == nil {
		t.Fatal("LowestFreeSubnetIndex() succeeded with no free subnets")
	}
}

func TestNodeMutationKeepsNamesAndCountsStable(t *testing.T) {
	t.Parallel()

	c, err := New("demo", 1, 1, 1, NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveNode(&c, "demo-worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if removed.Role != RoleWorker || c.Workers != 0 {
		t.Fatalf("removed = %#v, workers = %d", removed, c.Workers)
	}
	added, err := AddNode(&c, RoleWorker, "")
	if err != nil {
		t.Fatal(err)
	}
	if added.Name != "demo-worker-1" || c.Workers != 1 {
		t.Fatalf("added = %#v, workers = %d", added, c.Workers)
	}
	controlPlane, err := AddNode(&c, RoleControlPlane, "")
	if err != nil {
		t.Fatal(err)
	}
	if controlPlane.Name != "demo-cp-2" || c.ControlPlanes != 2 {
		t.Fatalf("added = %#v, control planes = %d", controlPlane, c.ControlPlanes)
	}
}

func TestDefaultsFor(t *testing.T) {
	base := NodeDefaults{MemoryMiB: 2048, CPUs: 2, DiskGiB: 20}
	cp := NodeDefaults{MemoryMiB: 3072, CPUs: 2, DiskGiB: 20}
	c := Cluster{
		NodeDefaults:         base,
		ControlPlaneDefaults: &cp,
	}
	if got := c.DefaultsFor(RoleControlPlane); got != cp {
		t.Errorf("control plane defaults = %+v, want override %+v", got, cp)
	}
	if got := c.DefaultsFor(RoleWorker); got != base {
		t.Errorf("worker defaults = %+v, want base %+v", got, base)
	}
}
