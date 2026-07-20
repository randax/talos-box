package manifests

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite golden files")

func facts() Facts {
	return Facts{Cluster: "demo", SubnetIndex: 0}
}

func TestGolden(t *testing.T) {
	tests := []struct {
		name   string
		render func(Facts) string
	}{
		{"lb-pool", LBPool},
		{"bgp", BGPPolicy},
		{"mirrors", RegistryMirrors},
		{"balloon", BalloonModule},
		{"all", All},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.render(facts())
			path := filepath.Join("testdata", tt.name+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden file (run with -update): %v", err)
			}
			if got != string(want) {
				t.Errorf("%s drifted from golden:\n%s", tt.name, got)
			}
		})
	}
}

// Every rendered section must be parseable YAML in all its documents.
func TestRenderedYAMLParses(t *testing.T) {
	for _, section := range []string{"lb-pool", "bgp", "mirrors", "balloon", "k8s", "talos", "all"} {
		t.Run(section, func(t *testing.T) {
			out, err := Render(facts(), section)
			if err != nil {
				t.Fatal(err)
			}
			decoder := yaml.NewDecoder(strings.NewReader(out))
			docs := 0
			for {
				var doc any
				err := decoder.Decode(&doc)
				if err != nil {
					if err.Error() == "EOF" {
						break
					}
					t.Fatalf("doc %d does not parse: %v", docs, err)
				}
				docs++
			}
			if docs == 0 {
				t.Fatal("no documents rendered")
			}
		})
	}
}

func TestSubnetValuesFlowThrough(t *testing.T) {
	f := Facts{Cluster: "edge", SubnetIndex: 3}
	for _, tt := range []struct {
		render func(Facts) string
		wants  []string
	}{
		{LBPool, []string{"172.30.3.200", "172.30.3.239", "edge"}},
		{BGPPolicy, []string{"64603", "64512", "172.30.3.1"}},
		{RegistryMirrors, []string{"http://172.30.3.1:5055", "http://172.30.3.1:5058", "registry.k8s.io"}},
		{BalloonModule, []string{"virtio_balloon"}},
	} {
		out := tt.render(f)
		for _, want := range tt.wants {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	}
}

func TestRenderUnknownSection(t *testing.T) {
	if _, err := Render(facts(), "nope"); err == nil {
		t.Fatal("expected error for unknown section")
	}
}
