package daemon

import (
	"encoding/json"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
)

func TestCreateFromSpecWithoutCNIUsesLegacyProvisioningFields(t *testing.T) {
	spec := config.ClusterSpec{Name: "demo"}
	args := createArgsFromSpec(spec, config.TalosSpec{}, false)
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cni", "lb", "bgp", "hubble"} {
		if _, found := fields[key]; found {
			t.Fatalf("createFromSpec legacy input unexpectedly includes %q: %s", key, encoded)
		}
	}
}

func TestSynchronizeHubbleIntentChangesOnlyTheCiliumHubbleToggle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true, Hubble: true}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	spec := config.ClusterSpec{Name: item.Name, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}}
	if err := synchronizeHubbleIntent(spec); err != nil {
		t.Fatal(err)
	}
	updated, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Hubble || !updated.BGP || !updated.LB || updated.CNI != cluster.CNICilium {
		t.Fatalf("updated provisioning intent = %+v", updated.ProvisioningIntent)
	}
	spec.Hubble = true
	if err := synchronizeHubbleIntent(spec); err != nil {
		t.Fatal(err)
	}
	updated, err = cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Hubble {
		t.Fatal("Hubble did not re-enable on the next reconciliation")
	}
}
