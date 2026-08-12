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
	for _, key := range []string{"cni", "csi", "lb", "bgp", "hubble"} {
		if _, found := fields[key]; found {
			t.Fatalf("createFromSpec legacy input unexpectedly includes %q: %s", key, encoded)
		}
	}
}

func TestCreateFromSpecPreservesCSIIntentOnTheWire(t *testing.T) {
	spec := config.ClusterSpec{
		Name: "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{
			CNI: cluster.CNICilium, CSI: cluster.CSILonghorn, LB: true,
		},
	}
	encoded, err := json.Marshal(createArgsFromSpec(spec, config.TalosSpec{}, false))
	if err != nil {
		t.Fatal(err)
	}
	var input cluster.ProvisioningIntentInput
	if err := json.Unmarshal(encoded, &input); err != nil {
		t.Fatal(err)
	}
	intent, err := input.Intent()
	if err != nil {
		t.Fatal(err)
	}
	if intent != spec.ProvisioningIntent {
		t.Fatalf("wire intent = %+v, want %+v", intent, spec.ProvisioningIntent)
	}
}
