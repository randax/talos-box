package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestReconcileProvisioningIntentMutationRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		current        cluster.ProvisioningIntent
		desired        cluster.ProvisioningIntent
		allMaintenance bool
		want           cluster.ProvisioningIntent
		wantChanged    bool
		wantErr        string
	}{
		{
			name:    "provisioned CNI cannot change",
			current: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			desired: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			wantErr: "tbx cluster destroy demo && tbx up",
		},
		{
			name:    "provisioned CNI cannot be removed",
			current: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			wantErr: "tbx cluster destroy demo && tbx up",
		},
		{
			name:           "add CNI while every node is in maintenance",
			desired:        cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			allMaintenance: true,
			want:           cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			wantChanged:    true,
		},
		{
			name:    "adding CNI after configuration is rejected",
			desired: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			wantErr: "all nodes are in maintenance",
		},
		{
			name:        "enable LoadBalancer later",
			current:     cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
			desired:     cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			want:        cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			wantChanged: true,
		},
		{
			name:    "disable LoadBalancer is rejected",
			current: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
			desired: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
			wantErr: "lb is immutable once enabled",
		},
		{
			name:        "Hubble remains symmetric",
			current:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, Hubble: true},
			desired:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			want:        cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			wantChanged: true,
		},
		{
			name:        "BGP becomes enabled declaratively",
			current:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			desired:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true},
			want:        cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true},
			wantChanged: true,
		},
		{
			name:        "BGP becomes disabled declaratively",
			current:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true},
			desired:     cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			want:        cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			wantChanged: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := cluster.Cluster{Name: "demo", ProvisioningIntent: test.current}
			got, changed, err := reconcileProvisioningIntent(item, config.ClusterSpec{Name: item.Name, ProvisioningIntent: test.desired}, test.allMaintenance)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("reconcileProvisioningIntent() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want || changed != test.wantChanged {
				t.Fatalf("reconcileProvisioningIntent() = (%+v, %t), want (%+v, %t)", got, changed, test.want, test.wantChanged)
			}
		})
	}
}

func TestPreflightUpRejectsEveryInvalidClusterBeforeAnyIntentIsPersisted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	first, err := cluster.New("first", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(first); err != nil {
		t.Fatal(err)
	}
	second, err := cluster.New("second", 1, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	second.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}
	if err := cluster.Save(second); err != nil {
		t.Fatal(err)
	}

	service := &Server{}
	_, err = service.preflightUp([]config.ClusterSpec{
		{Name: first.Name, ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}},
		{Name: second.Name, ProvisioningIntent: cluster.ProvisioningIntent{}},
	}, map[string]ClusterState{first.Name: {Exists: true}, second.Name: {Exists: true}}, func(cluster.Cluster) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "tbx cluster destroy second && tbx up") {
		t.Fatalf("preflightUp() error = %v, want immutable-CNI error", err)
	}
	updated, err := cluster.Load(first.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CNI != "" {
		t.Fatalf("preflight persisted first cluster before second failed: %+v", updated.ProvisioningIntent)
	}
}

func TestClusterStateRemainsIntentOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(filepath.Join(dir, "cluster.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"progress", "stage", "bootstrap", "provisioned"} {
		if strings.Contains(string(state), "\""+forbidden+"\"") {
			t.Fatalf("cluster state records provisioning progress %q:\n%s", forbidden, state)
		}
	}
}

func TestPersistIntentUpdatesDefersHostBGPDisableUntilL2Reconciliation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true, BGP: true}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	next := item
	next.BGP = false
	if err := persistIntentUpdates([]intentUpdate{{previous: item, next: next}}); err != nil {
		t.Fatal(err)
	}
	updated, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.BGP {
		t.Fatalf("persisted intent = %+v, want BGP disabled", updated.ProvisioningIntent)
	}
}
