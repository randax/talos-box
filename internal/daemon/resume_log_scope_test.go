package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// TestResumeColdBootLogLineIsClusterScoped pins #411: the cold-boot warning
// sends the operator to the daemon log for the verbatim hypervisor cause, and
// `tbx logs <cluster>` filters on the cluster name — so the line must name the
// node under its cluster ("resume <cluster>/<node>: …"), not the node alone.
func TestResumeColdBootLogLineIsClusterScoped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("fake-scope", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
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
	savePath := filepath.Join(dir, item.Nodes[0].Name+".vzstate")
	if err := os.WriteFile(savePath, []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	previous := log.Writer()
	flags := log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(previous); log.SetFlags(flags) })

	backend := &fakeHypervisor{launch: func(_ context.Context, spec hypervisor.Spec) (hypervisor.Machine, error) {
		spec.Restore.Fallback(hypervisor.ErrIncompatibleSave)
		return &fakeMachine{active: true}, nil
	}}
	service := &Server{
		hypervisors:   singleFakeRegistry(backend),
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: emptySubnetSources(),
	}
	if _, err := service.resumeCluster([]byte(`{"name":"fake-scope"}`)); err != nil {
		t.Fatal(err)
	}

	want := "resume " + item.Name + "/" + item.Nodes[0].Name + ":"
	if !strings.Contains(logged.String(), want) {
		t.Fatalf("daemon log = %q, want a line with subject %q", logged.String(), want)
	}
}
