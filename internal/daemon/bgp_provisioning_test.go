package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

type legacyBGPClient struct {
	enabled bool
}

func (c *legacyBGPClient) EnableBGP(string, int, uint32, uint32) error { c.enabled = true; return nil }
func (*legacyBGPClient) DisableBGP(string) error                       { return nil }
func (*legacyBGPClient) Close() error                                  { return nil }

func TestSetBGPRepairsMigratedLegacyState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("legacy-bgp", 2, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.BGP = true
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "cluster.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"cni"`) {
		t.Fatalf("legacy fixture unexpectedly has cni: %s", data)
	}

	client := &legacyBGPClient{}
	original := connectBGPHelper
	connectBGPHelper = func() (bgpHelperClient, error) { return client, nil }
	t.Cleanup(func() { connectBGPHelper = original })
	raw, err := json.Marshal(nameArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&Server{}).setBGP(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if !client.enabled || result.CNI != cluster.CNICilium || !result.LB || !result.BGP {
		t.Fatalf("migrated BGP result = %+v, helper enabled=%t", result, client.enabled)
	}
}

func TestSetBGPRejectsIntentThatCannotUseBGP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		name    string
		intent  cluster.ProvisioningIntent
		wantErr string
	}{
		{"substrate only", cluster.ProvisioningIntent{}, "bgp requires cni"},
		{"flannel", cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}, "bgp requires cni: cilium"},
		{"load balancer disabled", cluster.ProvisioningIntent{CNI: cluster.CNICilium}, "bgp requires lb: true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			item.ProvisioningIntent = tt.intent
			if err := cluster.Save(item); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(nameArgs{Name: item.Name})
			if err != nil {
				t.Fatal(err)
			}
			_, err = (&Server{}).setBGP(raw, true)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("setBGP() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
