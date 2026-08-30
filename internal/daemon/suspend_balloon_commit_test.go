package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// A suspend whose balloon marker cannot be settled is a failed suspend: the
// save is removed and the node stays tracked, instead of a "successful" save
// the next resume would misclassify and cold-boot (#513).
func TestSuspendFailsWhenBalloonMarkerCannotCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("marker-fail", 0, 1, 0, cluster.NodeDefaults{MemoryMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	node := item.Nodes[0].Name
	savePath := saveStatePath(dir, node)
	// the marker path is a non-empty directory, so the balloon-less save
	// cannot stamp it
	if err := os.MkdirAll(filepath.Join(saveStateBalloonPath(savePath), "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	machine := &fakeMachine{active: true, onSuspend: func(path string) error { return os.WriteFile(path, []byte("mem"), 0o600) }}
	service := &Server{
		hypervisor: &fakeHypervisor{capabilities: hypervisor.Capabilities{
			Suspend: hypervisor.FeatureStatus{Supported: true},
		}},
		vms:             map[string]map[string]hypervisor.Machine{item.Name: {node: machine}},
		balloonDisabled: true,
	}
	raw, _ := json.Marshal(nameArgs{Name: item.Name})
	if _, err = service.suspendCluster(raw); err == nil {
		t.Fatal("suspendCluster succeeded with an uncommittable balloon marker")
	}
	if _, statErr := os.Stat(savePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("save left behind after failed suspend: stat err = %v", statErr)
	}
	if service.vms[item.Name][node] == nil {
		t.Fatal("node dropped from tracking after a failed suspend; it must stay retained")
	}
}
