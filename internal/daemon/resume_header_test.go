package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

func seedQEMUSavedCluster(t *testing.T, name string) (cluster.Cluster, string) {
	t.Helper()
	item, err := cluster.New(name, 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	item.Hypervisor = string(hypervisor.NameQEMU)
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
	return item, savePath
}

func TestResumeRefusesIncompatibleHeaderAndPreservesSave(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, savePath := seedQEMUSavedCluster(t, "header-refusal")
	backend := &fakeHypervisor{launch: func(_ context.Context, spec hypervisor.Spec) (hypervisor.Machine, error) {
		if spec.Restore == nil || spec.Restore.Path != savePath {
			t.Fatalf("Restore = %+v, want path %q", spec.Restore, savePath)
		}
		return nil, fmt.Errorf("launch restore: %w: save uses machine mismatch", hypervisor.ErrIncompatibleSave)
	}}
	service := &Server{
		hypervisors: fakeRegistry(hypervisor.NameQEMU, map[hypervisor.Name]hypervisor.Hypervisor{
			hypervisor.NameQEMU: backend,
		}),
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: emptySubnetSources(),
	}

	_, err := service.resumeCluster(mustRawJSON(t, startArgs{Name: item.Name}))
	if !errors.Is(err, hypervisor.ErrIncompatibleSave) {
		t.Fatalf("resumeCluster() = %v, want ErrIncompatibleSave", err)
	}
	if _, err := os.Stat(savePath); err != nil {
		t.Fatalf("refused resume removed retryable save: %v", err)
	}
}

func TestNewDaemonResumesQEMUFromDiskWithoutColdBoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, savePath := seedQEMUSavedCluster(t, "new-daemon-resume")
	suspendedAt := time.Now().Add(-12 * time.Minute)
	if err := os.Chtimes(savePath, suspendedAt, suspendedAt); err != nil {
		t.Fatal(err)
	}
	backend := &fakeHypervisor{launch: func(_ context.Context, spec hypervisor.Spec) (hypervisor.Machine, error) {
		if spec.Restore == nil || spec.Restore.Path != savePath {
			t.Fatalf("Restore = %+v, want path %q", spec.Restore, savePath)
		}
		return &fakeMachine{active: true}, nil
	}}
	service := &Server{
		hypervisors: fakeRegistry(hypervisor.NameQEMU, map[hypervisor.Name]hypervisor.Hypervisor{
			hypervisor.NameQEMU: backend,
		}),
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: emptySubnetSources(),
	}

	result, err := service.resumeCluster(mustRawJSON(t, startArgs{Name: item.Name}))
	if err != nil {
		t.Fatal(err)
	}
	warnings := strings.Join(result.Warnings, "\n")
	if strings.Contains(strings.ToLower(warnings), "cold-boot") {
		t.Fatalf("warnings = %q, want no cold-boot warning", warnings)
	}
	if !strings.Contains(warnings, "behind the host") {
		t.Fatalf("warnings = %q, want clock-drift note", warnings)
	}
	if _, err := os.Stat(savePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful restore left consumed save on disk: %v", err)
	}
}
