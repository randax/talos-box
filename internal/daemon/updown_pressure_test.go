package daemon

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/vm"
)

func TestUpForceSurfacesHostPressureWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("forced-up", 0, 0, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms:          make(map[string]map[string]*vm.VM),
		hostPressure: extremeSwapPressure,
		subnetSources: cluster.SubnetSources{
			Interfaces: func() ([]cluster.HostInterface, error) { return nil, nil },
			Route:      func(net.IP) (cluster.HostRoute, error) { return cluster.HostRoute{}, nil },
		},
	}
	raw, err := json.Marshal(upArgs{
		Clusters: []config.ClusterSpec{{Name: item.Name}},
		Force:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	actions, err := service.up(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != ActionStart {
		t.Fatalf("up() actions = %+v, want one start", actions)
	}
	for _, fragment := range []string{"host swap is 90% used", "(forced)"} {
		if !strings.Contains(actions[0].Warning, fragment) {
			t.Errorf("Action.Warning = %q, missing %q", actions[0].Warning, fragment)
		}
	}
}
