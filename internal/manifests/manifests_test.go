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
		{"cilium-values", CiliumValues},
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
	for _, section := range []string{"lb-pool", "bgp", "cilium-values", "mirrors", "balloon", "k8s", "talos", "all"} {
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

func TestBGPPolicyMatchesCilium119Schema(t *testing.T) {
	docs := decodeYAMLDocuments(t, BGPPolicy(facts()))
	if len(docs) != 3 {
		t.Fatalf("BGPPolicy rendered %d documents, want 3", len(docs))
	}

	byKind := make(map[string]map[string]any, len(docs))
	for _, doc := range docs {
		kind, _ := doc["kind"].(string)
		byKind[kind] = doc
		if got := doc["apiVersion"]; got != "cilium.io/v2" {
			t.Errorf("%s apiVersion = %v, want cilium.io/v2", kind, got)
		}
	}

	cluster := byKind["CiliumBGPClusterConfig"]
	peer := byKind["CiliumBGPPeerConfig"]
	advertisement := byKind["CiliumBGPAdvertisement"]
	if cluster == nil || peer == nil || advertisement == nil {
		t.Fatalf("BGPPolicy kinds = %v, want CiliumBGPClusterConfig, CiliumBGPPeerConfig, and CiliumBGPAdvertisement", mapKeys(byKind))
	}

	clusterConfig := decodeKnownFields[ciliumBGPClusterConfig](t, cluster)
	if clusterConfig.Metadata.Name != "demo-bgp" || len(clusterConfig.Spec.BGPInstances) != 1 {
		t.Fatalf("cluster config = %+v", clusterConfig)
	}
	instance := clusterConfig.Spec.BGPInstances[0]
	if instance.Name != "demo-bgp" || instance.LocalASN != 64600 || len(instance.Peers) != 1 {
		t.Fatalf("BGP instance = %+v", instance)
	}
	clusterPeer := instance.Peers[0]
	if clusterPeer.Name != "host-gateway" || clusterPeer.PeerASN != 64512 || clusterPeer.PeerAddress != "172.30.0.1" || clusterPeer.PeerConfigRef.Name != "demo-bgp-peer" {
		t.Errorf("BGP peer = %+v", clusterPeer)
	}

	peerConfig := decodeKnownFields[ciliumBGPPeerConfig](t, peer)
	if peerConfig.Metadata.Name != "demo-bgp-peer" || len(peerConfig.Spec.Families) != 1 {
		t.Fatalf("peer config = %+v", peerConfig)
	}
	family := peerConfig.Spec.Families[0]
	if family.AFI != "ipv4" || family.SAFI != "unicast" || family.Advertisements.MatchLabels[advertisementLabel] != "service-load-balancer" {
		t.Errorf("BGP family = %+v", family)
	}

	advertisementConfig := decodeKnownFields[ciliumBGPAdvertisement](t, advertisement)
	if advertisementConfig.Metadata.Labels[advertisementLabel] != "service-load-balancer" || len(advertisementConfig.Spec.Advertisements) != 1 {
		t.Fatalf("advertisement config = %+v", advertisementConfig)
	}
	serviceAdvertisement := advertisementConfig.Spec.Advertisements[0]
	if serviceAdvertisement.AdvertisementType != "Service" || len(serviceAdvertisement.Service.Addresses) != 1 || serviceAdvertisement.Service.Addresses[0] != "LoadBalancerIP" {
		t.Errorf("service advertisement = %+v", serviceAdvertisement)
	}
	if len(serviceAdvertisement.Selector.MatchExpressions) != 1 || serviceAdvertisement.Selector.MatchExpressions[0].Operator != "NotIn" {
		t.Errorf("service selector = %+v", serviceAdvertisement.Selector)
	}
}

func TestCiliumValuesSizeClientRateLimitForL2Announcements(t *testing.T) {
	for _, tt := range []struct {
		name      string
		bgp       bool
		wantL2    bool
		wantLimit bool
	}{
		{name: "L2 mode enables announcements with sized limit", wantL2: true, wantLimit: true},
		{name: "BGP mode disables L2 announcements", bgp: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := facts()
			f.BGP = tt.bgp
			out, err := Render(f, "cilium-values")
			if err != nil {
				t.Fatal(err)
			}

			var values struct {
				L2Announcements struct {
					Enabled bool `yaml:"enabled"`
				} `yaml:"l2announcements"`
				ClientRateLimit struct {
					QPS   int `yaml:"qps"`
					Burst int `yaml:"burst"`
				} `yaml:"k8sClientRateLimit"`
			}
			if err := yaml.Unmarshal([]byte(out), &values); err != nil {
				t.Fatal(err)
			}
			if values.L2Announcements.Enabled != tt.wantL2 {
				t.Errorf("L2 announcements enabled = %t, want %t", values.L2Announcements.Enabled, tt.wantL2)
			}
			if tt.wantLimit && (values.ClientRateLimit.QPS != 10 || values.ClientRateLimit.Burst != 20) {
				t.Errorf("client rate limit = %d QPS/%d burst, want 10 QPS/20 burst", values.ClientRateLimit.QPS, values.ClientRateLimit.Burst)
			}
		})
	}
}

func decodeYAMLDocuments(t *testing.T, rendered string) []map[string]any {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(rendered))
	var docs []map[string]any
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				return docs
			}
			t.Fatalf("decode rendered YAML: %v", err)
		}
		docs = append(docs, doc)
	}
}

func decodeKnownFields[T any](t *testing.T, value any) T {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	var decoded T
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("manifest does not match the Cilium 1.19 schema: %v", err)
	}
	return decoded
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

const advertisementLabel = "talosbox.dev/advertisement"

// These test-only types mirror the fields talosbox uses from Cilium 1.19.6's
// v2 BGP CRD schemas. KnownFields rejects misspelled or misplaced fields, while
// the assertions above enforce the enums and required relationships we emit.
type ciliumTypeMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

type ciliumObjectMeta struct {
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels,omitempty"`
}

type ciliumLabelSelector struct {
	MatchLabels      map[string]string               `yaml:"matchLabels,omitempty"`
	MatchExpressions []ciliumLabelSelectorExpression `yaml:"matchExpressions,omitempty"`
}

type ciliumLabelSelectorExpression struct {
	Key      string   `yaml:"key"`
	Operator string   `yaml:"operator"`
	Values   []string `yaml:"values,omitempty"`
}

type ciliumBGPClusterConfig struct {
	ciliumTypeMeta `yaml:",inline"`
	Metadata       ciliumObjectMeta `yaml:"metadata"`
	Spec           struct {
		NodeSelector ciliumLabelSelector `yaml:"nodeSelector"`
		BGPInstances []struct {
			Name     string `yaml:"name"`
			LocalASN int    `yaml:"localASN"`
			Peers    []struct {
				Name          string `yaml:"name"`
				PeerASN       int    `yaml:"peerASN"`
				PeerAddress   string `yaml:"peerAddress"`
				PeerConfigRef struct {
					Name string `yaml:"name"`
				} `yaml:"peerConfigRef"`
			} `yaml:"peers"`
		} `yaml:"bgpInstances"`
	} `yaml:"spec"`
}

type ciliumBGPPeerConfig struct {
	ciliumTypeMeta `yaml:",inline"`
	Metadata       ciliumObjectMeta `yaml:"metadata"`
	Spec           struct {
		Families []struct {
			AFI            string              `yaml:"afi"`
			SAFI           string              `yaml:"safi"`
			Advertisements ciliumLabelSelector `yaml:"advertisements"`
		} `yaml:"families"`
	} `yaml:"spec"`
}

type ciliumBGPAdvertisement struct {
	ciliumTypeMeta `yaml:",inline"`
	Metadata       ciliumObjectMeta `yaml:"metadata"`
	Spec           struct {
		Advertisements []struct {
			AdvertisementType string `yaml:"advertisementType"`
			Service           struct {
				Addresses []string `yaml:"addresses"`
			} `yaml:"service"`
			Selector ciliumLabelSelector `yaml:"selector"`
		} `yaml:"advertisements"`
	} `yaml:"spec"`
}

func TestSubnetValuesFlowThrough(t *testing.T) {
	f := Facts{Cluster: "edge", SubnetIndex: 3}
	for _, tt := range []struct {
		render func(Facts) string
		wants  []string
	}{
		{LBPool, []string{"172.30.3.200", "172.30.3.239", "edge"}},
		{BGPPolicy, []string{"64603", "64512", "172.30.3.1"}},
		{CiliumValues, []string{"172.30.3.200", "qps: 10", "burst: 20"}},
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
