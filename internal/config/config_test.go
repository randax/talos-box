package config

import (
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

func TestParseDefaults(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []ClusterSpec
	}{
		{
			name: "minimal cluster gets all defaults",
			yaml: "version: 1\nclusters:\n  - name: demo\n",
			want: []ClusterSpec{{
				Name:          "demo",
				ControlPlanes: 1,
				Workers:       2,
				Node:          cluster.NodeDefaults{MemoryMiB: 2048, CPUs: 2, DiskGiB: 20},
			}},
		},
		{
			name: "explicit counts and sizes survive",
			yaml: `version: 1
clusters:
  - name: big
    controlPlanes: 3
    workers: 0
    node:
      memory: 4GiB
      cpus: 4
      diskSize: 40GiB
`,
			want: []ClusterSpec{{
				Name:          "big",
				ControlPlanes: 3,
				Workers:       0,
				Node:          cluster.NodeDefaults{MemoryMiB: 4096, CPUs: 4, DiskGiB: 40},
			}},
		},
		{
			name: "per-role override merges field-wise over node defaults",
			yaml: `version: 1
clusters:
  - name: mixed
    node:
      memory: 2GiB
    controlPlane:
      memory: 3GiB
    worker:
      cpus: 4
`,
			want: []ClusterSpec{{
				Name:          "mixed",
				ControlPlanes: 1,
				Workers:       2,
				Node:          cluster.NodeDefaults{MemoryMiB: 2048, CPUs: 2, DiskGiB: 20},
				ControlPlane:  &cluster.NodeDefaults{MemoryMiB: 3072, CPUs: 2, DiskGiB: 20},
				Worker:        &cluster.NodeDefaults{MemoryMiB: 2048, CPUs: 4, DiskGiB: 20},
			}},
		},
		{
			name: "MiB sizes accepted",
			yaml: "version: 1\nclusters:\n  - name: lean\n    node: {memory: 1536MiB}\n",
			want: []ClusterSpec{{
				Name:          "lean",
				ControlPlanes: 1,
				Workers:       2,
				Node:          cluster.NodeDefaults{MemoryMiB: 1536, CPUs: 2, DiskGiB: 20},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(cfg.Clusters) != len(tt.want) {
				t.Fatalf("got %d clusters, want %d", len(cfg.Clusters), len(tt.want))
			}
			for i, want := range tt.want {
				got := cfg.Clusters[i]
				if got.Name != want.Name || got.ControlPlanes != want.ControlPlanes || got.Workers != want.Workers {
					t.Errorf("cluster %d shape = %+v, want %+v", i, got, want)
				}
				if got.Node != want.Node {
					t.Errorf("cluster %d node = %+v, want %+v", i, got.Node, want.Node)
				}
				if (got.ControlPlane == nil) != (want.ControlPlane == nil) ||
					(got.ControlPlane != nil && *got.ControlPlane != *want.ControlPlane) {
					t.Errorf("cluster %d controlPlane = %+v, want %+v", i, got.ControlPlane, want.ControlPlane)
				}
				if (got.Worker == nil) != (want.Worker == nil) ||
					(got.Worker != nil && *got.Worker != *want.Worker) {
					t.Errorf("cluster %d worker = %+v, want %+v", i, got.Worker, want.Worker)
				}
			}
		})
	}
}

func TestParseTalosDefaults(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nclusters:\n  - name: demo\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Talos.Version != "" || cfg.Talos.Schematic != "" {
		t.Errorf("talos spec should stay empty (daemon resolves defaults), got %+v", cfg.Talos)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"unsupported version", "version: 2\nclusters: [{name: a}]", "version"},
		{"no clusters", "version: 1\n", "at least one cluster"},
		{"empty name", "version: 1\nclusters: [{name: \"\"}]", "name"},
		{"duplicate names", "version: 1\nclusters: [{name: a}, {name: a}]", "duplicate"},
		{"invalid name", "version: 1\nclusters: [{name: \"Bad_Name\"}]", "name"},
		{"zero control planes", "version: 1\nclusters: [{name: a, controlPlanes: 0}]", "control plane"},
		{"bad size unit", "version: 1\nclusters: [{name: a, node: {memory: 2GB}}]", "size"},
		{"garbage size", "version: 1\nclusters: [{name: a, node: {diskSize: lots}}]", "size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	spec := ClusterSpec{
		Name: "demo", ControlPlanes: 1, Workers: 2,
		Node: cluster.NodeDefaults{MemoryMiB: 2048, CPUs: 2, DiskGiB: 20},
	}
	got := Marshal(Config{Clusters: []ClusterSpec{spec}})
	want := `version: 1
clusters:
  - name: demo
    controlPlanes: 1
    workers: 2
    node:
      memory: 2GiB
      cpus: 2
      diskSize: 20GiB
`
	if got != want {
		t.Errorf("Marshal:\n%s\nwant:\n%s", got, want)
	}
	// what we print must be what we can parse
	back, err := Parse([]byte(got))
	if err != nil {
		t.Fatalf("re-Parse of marshaled config: %v", err)
	}
	if back.Clusters[0] != spec {
		t.Errorf("round trip = %+v, want %+v", back.Clusters[0], spec)
	}
}
