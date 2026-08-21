package config

import (
	"reflect"
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

func TestParseProvisioningIntent(t *testing.T) {
	cfg, err := Parse([]byte(`version: 1
clusters:
  - name: cilium
    cni: cilium
    csi: longhorn
    bgp: true
    hubble: true
  - name: flannel
    cni: flannel
    lb: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters[0]; got.CNI != cluster.CNICilium || got.CSI != cluster.CSILonghorn || !got.LB || !got.BGP || !got.Hubble {
		t.Fatalf("cilium provisioning intent = %+v", got)
	}
	if got := cfg.Clusters[1]; got.CNI != cluster.CNIFlannel || got.LB || got.BGP || got.Hubble {
		t.Fatalf("flannel provisioning intent = %+v", got)
	}
}

func TestParseRejectsInvalidProvisioningIntent(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"unknown cni", "cni: calico", "cni must be one of"},
		{"unknown csi", "cni: cilium\n    csi: rook", "csi must be one of longhorn | local-path"},
		{"csi without cni", "csi: longhorn", "add cni:"},
		{"lb without cni", "lb: false", "lb requires cni"},
		{"bgp without cni", "bgp: false", "bgp requires cni"},
		{"hubble without cni", "hubble: false", "hubble requires cni"},
		{"bgp without load balancer", "cni: cilium\n    lb: false\n    bgp: true", "bgp requires lb: true"},
		{"flannel bgp", "cni: flannel\n    bgp: true", "bgp requires cni: cilium"},
		{"flannel hubble", "cni: flannel\n    hubble: true", "hubble requires cni: cilium"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte("version: 1\nclusters:\n  - name: demo\n    " + tt.yaml + "\n"))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Parse() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"unsupported version", "version: 2\nclusters: [{name: a}]", "unsupported version 2 (this tbx understands version 1)"},
		{"missing version", "clusters: [{name: a}]", "talosbox.yaml is missing 'version:'; add 'version: 1'"},
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

func TestParseDomainFields(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nclusters:\n  - name: demo\n    domain: Lab.Internal.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters[0].Domain; got != "lab.internal" {
		t.Errorf("domain = %q, want canonical %q", got, "lab.internal")
	}
	if cfg.Clusters[0].AllowUnsafeDomain {
		t.Error("allowUnsafeDomain should default to false")
	}
}

func TestParseDomainErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"rejected TLD", "version: 1\nclusters: [{name: a, domain: a.local}]", "mDNS"},
		{"unsafe without opt-in", "version: 1\nclusters: [{name: a, domain: corp.example.com}]", "allow-unsafe-domain"},
		{"duplicate explicit domains", "version: 1\nclusters: [{name: a, domain: lab.test}, {name: b, domain: lab.test}]", "domain"},
		{"explicit collides with default", "version: 1\nclusters: [{name: a}, {name: b, domain: a.k8s.test}]", "domain"},
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

func TestParseRejectsNameFormingInvalidDefaultDomain(t *testing.T) {
	long := strings.Repeat("a", 64) // valid for nameRe, invalid as a DNS label
	_, err := Parse([]byte("version: 1\nclusters: [{name: " + long + "}]"))
	if err == nil {
		t.Fatal("Parse accepted a 64-char cluster name whose default domain is invalid")
	}
}

func TestParseNormalizesExplicitOwnDefaultDomain(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nclusters:\n  - name: demo\n    domain: demo.k8s.test\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters[0].Domain; got != "" {
		t.Fatalf("domain = %q, want empty (own default normalizes away)", got)
	}
}

func TestParseUnsafeDomainOptIn(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nclusters:\n  - name: demo\n    domain: corp.example.com\n    allowUnsafeDomain: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters[0].Domain; got != "corp.example.com" {
		t.Errorf("domain = %q, want %q", got, "corp.example.com")
	}
	if !cfg.Clusters[0].AllowUnsafeDomain {
		t.Error("allowUnsafeDomain not carried through")
	}
}

func TestMarshalEmitsDomainOnlyWhenSet(t *testing.T) {
	base := ClusterSpec{Name: "demo", ControlPlanes: 1, Workers: 2, Node: cluster.NodeDefaults{MemoryMiB: 2048, CPUs: 2, DiskGiB: 20}}
	if out := Marshal(Config{Clusters: []ClusterSpec{base}}); strings.Contains(out, "domain") {
		t.Errorf("Marshal emitted domain for default cluster:\n%s", out)
	}

	withDomain := base
	withDomain.Domain = "corp.example.com"
	withDomain.AllowUnsafeDomain = true
	out := Marshal(Config{Clusters: []ClusterSpec{withDomain}})
	if !strings.Contains(out, "domain: corp.example.com\n") || !strings.Contains(out, "allowUnsafeDomain: true\n") {
		t.Errorf("Marshal missing domain fields:\n%s", out)
	}
	back, err := Parse([]byte(out))
	if err != nil {
		t.Fatalf("re-Parse of marshaled config: %v", err)
	}
	if !reflect.DeepEqual(back.Clusters[0], withDomain) {
		t.Errorf("round trip = %+v, want %+v", back.Clusters[0], withDomain)
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
	if !reflect.DeepEqual(back.Clusters[0], spec) {
		t.Errorf("round trip = %+v, want %+v", back.Clusters[0], spec)
	}
}

func TestMarshalProvisioningIntentRoundTrip(t *testing.T) {
	spec := ClusterSpec{
		Name: "demo", ControlPlanes: 1, Workers: 2,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILocalPath, LB: true, BGP: true, Hubble: true},
		Node:               cluster.NodeDefaults{MemoryMiB: 2048, CPUs: 2, DiskGiB: 20},
	}
	got := Marshal(Config{Clusters: []ClusterSpec{spec}})
	for _, line := range []string{"cni: cilium", "csi: local-path", "lb: true", "bgp: true", "hubble: true"} {
		if !strings.Contains(got, line) {
			t.Fatalf("Marshal() missing %q:\n%s", line, got)
		}
	}
	back, err := Parse([]byte(got))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Clusters[0], spec) {
		t.Fatalf("round trip = %+v, want %+v", back.Clusters[0], spec)
	}
}

func TestParsePerClusterTalosInheritance(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []TalosSpec
	}{
		{
			name: "no talos anywhere stays empty",
			yaml: "version: 1\nclusters:\n  - name: demo\n",
			want: []TalosSpec{{}},
		},
		{
			name: "cluster without a talos block inherits the file-level block",
			yaml: `version: 1
talos:
  version: v1.13.6
  schematic: aaa
  extensions: [qemu-guest-agent]
clusters:
  - name: demo
`,
			want: []TalosSpec{{Version: "v1.13.6", Schematic: "aaa", Extensions: []string{"qemu-guest-agent"}}},
		},
		{
			name: "set fields override, unset fields inherit",
			yaml: `version: 1
talos:
  version: v1.13.6
  schematic: aaa
clusters:
  - name: canary
    talos:
      version: v1.14.0
  - name: custom
    talos:
      schematic: bbb
`,
			want: []TalosSpec{
				{Version: "v1.14.0", Schematic: "aaa"},
				{Version: "v1.13.6", Schematic: "bbb"},
			},
		},
		{
			name: "extension lists override rather than concatenate",
			yaml: `version: 1
talos:
  extensions: [qemu-guest-agent, gvisor]
clusters:
  - name: nfs
    talos:
      extensions: [nfs-utils]
`,
			want: []TalosSpec{{Extensions: []string{"nfs-utils"}}},
		},
		{
			name: "extensions [] is a meaningful opt-out",
			yaml: `version: 1
talos:
  extensions: [qemu-guest-agent]
clusters:
  - name: bare
    talos:
      extensions: []
  - name: inheriting
`,
			want: []TalosSpec{
				{Extensions: []string{}},
				{Extensions: []string{"qemu-guest-agent"}},
			},
		},
		{
			name: "empty per-cluster block inherits everything",
			yaml: `version: 1
talos:
  version: v1.13.6
  extensions: [gvisor]
clusters:
  - name: demo
    talos: {}
`,
			want: []TalosSpec{{Version: "v1.13.6", Extensions: []string{"gvisor"}}},
		},
		{
			name: "explicit null extensions means unset, so it inherits",
			yaml: `version: 1
talos:
  extensions: [gvisor]
clusters:
  - name: demo
    talos:
      extensions:
`,
			want: []TalosSpec{{Extensions: []string{"gvisor"}}},
		},
		{
			name: "per-cluster block without file-level block",
			yaml: `version: 1
clusters:
  - name: pinned
    talos:
      version: v1.14.1
      extensions: [gvisor]
  - name: plain
`,
			want: []TalosSpec{
				{Version: "v1.14.1", Extensions: []string{"gvisor"}},
				{},
			},
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
				got := cfg.Clusters[i].Talos
				if !reflect.DeepEqual(got, want) {
					t.Errorf("cluster %d talos = %#v, want %#v", i, got, want)
				}
			}
		})
	}
}

func TestParseFileLevelTalosExtensions(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\ntalos:\n  extensions: [gvisor]\nclusters:\n  - name: demo\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Talos.Extensions, []string{"gvisor"}) {
		t.Errorf("file-level extensions = %#v, want [gvisor]", cfg.Talos.Extensions)
	}
}

func TestParseRejectsUnknownTalosKeys(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"unknown key in file-level talos", "version: 1\ntalos:\n  flavour: spicy\nclusters:\n  - name: a\n"},
		{"unknown key in per-cluster talos", "version: 1\nclusters:\n  - name: a\n    talos:\n      flavour: spicy\n"},
		{"unknown key at cluster level", "version: 1\nclusters:\n  - name: a\n    talosVersion: v1.14.0\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.yaml)); err == nil {
				t.Fatal("expected error for unknown key, got nil")
			}
		})
	}
}

func TestTalosSpecEqualDistinguishesOptOutFromUnset(t *testing.T) {
	if (TalosSpec{Extensions: []string{}}).Equal(TalosSpec{}) {
		t.Error("explicit [] opt-out must not equal an unset extensions list")
	}
	if !(TalosSpec{Version: "v1", Extensions: []string{"a"}}).Equal(TalosSpec{Version: "v1", Extensions: []string{"a"}}) {
		t.Error("identical specs must be equal")
	}
}

func TestMarshalPerClusterTalosRoundTrip(t *testing.T) {
	fileTalos := TalosSpec{Version: "v1.13.6", Extensions: []string{"qemu-guest-agent"}}
	specs := []ClusterSpec{
		{
			Name: "stable", ControlPlanes: 1, Workers: 2,
			Node:  cluster.NodeDefaults{MemoryMiB: 2048, CPUs: 2, DiskGiB: 20},
			Talos: fileTalos,
		},
		{
			Name: "canary", ControlPlanes: 1, Workers: 2,
			Node:  cluster.NodeDefaults{MemoryMiB: 2048, CPUs: 2, DiskGiB: 20},
			Talos: TalosSpec{Version: "v1.14.0", Schematic: "bbb", Extensions: []string{}},
		},
	}
	out := Marshal(Config{Talos: fileTalos, Clusters: specs})
	back, err := Parse([]byte(out))
	if err != nil {
		t.Fatalf("re-Parse of marshaled config: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(back.Talos, fileTalos) {
		t.Errorf("file talos round trip = %#v, want %#v\n%s", back.Talos, fileTalos, out)
	}
	for i, spec := range specs {
		if !reflect.DeepEqual(back.Clusters[i].Talos, spec.Talos) {
			t.Errorf("cluster %d talos round trip = %#v, want %#v\n%s", i, back.Clusters[i].Talos, spec.Talos, out)
		}
	}
	// A cluster matching the file-level block must not repeat it.
	if strings.Count(out, "talos:") != 2 {
		t.Errorf("expected exactly one file-level and one per-cluster talos block:\n%s", out)
	}
}

func TestMarshalQuotesExtensionsCarryingFlowSyntax(t *testing.T) {
	spec := ClusterSpec{
		Name: "demo", ControlPlanes: 1, Workers: 2,
		Node:  cluster.NodeDefaults{MemoryMiB: 2048, CPUs: 2, DiskGiB: 20},
		Talos: TalosSpec{Extensions: []string{"foo,bar", "gvisor"}},
	}
	out := Marshal(Config{Clusters: []ClusterSpec{spec}})
	back, err := Parse([]byte(out))
	if err != nil {
		t.Fatalf("re-Parse of marshaled config: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(back.Clusters[0].Talos.Extensions, spec.Talos.Extensions) {
		t.Fatalf("extensions round trip = %#v, want %#v\n%s", back.Clusters[0].Talos.Extensions, spec.Talos.Extensions, out)
	}
}
