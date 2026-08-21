package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

type suspendMachineFake struct {
	calls      []string
	suspendErr error
	stopErr    error
	closeErr   error
}

func (f *suspendMachineFake) Active() bool { return true }

func (f *suspendMachineFake) SetMemoryTargetMiB(int) error { return nil }

func (f *suspendMachineFake) Suspend(_ context.Context, path string) error {
	f.calls = append(f.calls, "suspend "+path)
	return f.suspendErr
}

func (f *suspendMachineFake) Stop(context.Context) error {
	f.calls = append(f.calls, "stop")
	return f.stopErr
}

func (f *suspendMachineFake) Close() error {
	f.calls = append(f.calls, "close")
	return f.closeErr
}

func TestPrepareSavedMachineRetainsResourcesAfterStopping(t *testing.T) {
	machine := &suspendMachineFake{}

	retain, err := prepareSavedMachine(machine, "/tmp/node.vzstate")
	if err != nil {
		t.Fatal(err)
	}
	if !retain {
		t.Fatal("successfully saved machine must remain tracked for restore")
	}

	want := []string{"suspend /tmp/node.vzstate", "stop"}
	if !reflect.DeepEqual(machine.calls, want) {
		t.Fatalf("calls = %v, want %v", machine.calls, want)
	}
}

func TestSuspendInvalidatesStorageLiveObservation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	machine := &suspendMachineFake{}
	service := &Server{
		hypervisor:    &fakeHypervisor{capabilities: hypervisor.Capabilities{Suspend: hypervisor.FeatureStatus{Supported: true}}},
		vms:           map[string]map[string]hypervisor.Machine{item.Name: {item.Nodes[0].Name: machine}},
		storagePhases: map[string]StoragePhase{item.Name: StoragePhaseLive},
	}
	if _, err := service.suspendCluster([]byte(`{"name":"demo"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := service.storagePhases[item.Name]; ok {
		t.Fatal("suspend retained stale storage-live observation")
	}
}

func TestUnsupportedSuspendPreservesStorageLiveObservation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		hypervisor:    &fakeHypervisor{},
		vms:           map[string]map[string]hypervisor.Machine{item.Name: {item.Nodes[0].Name: &suspendMachineFake{}}},
		storagePhases: map[string]StoragePhase{item.Name: StoragePhaseLive},
	}
	if _, err := service.suspendCluster([]byte(`{"name":"demo"}`)); !errors.Is(err, hypervisor.ErrUnsupported) {
		t.Fatalf("suspend error = %v, want unsupported", err)
	}
	if service.storagePhases[item.Name] != StoragePhaseLive {
		t.Fatal("unsupported suspend discarded storage-live observation")
	}
}

func TestSuspendCancelsActiveProvisioning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Server{
		hypervisor: &fakeHypervisor{capabilities: hypervisor.Capabilities{Suspend: hypervisor.FeatureStatus{Supported: true}}},
		vms:        map[string]map[string]hypervisor.Machine{item.Name: {item.Nodes[0].Name: &suspendMachineFake{}}},
		provisions: map[string]activeProvision{item.Name: {generation: 1, cancel: cancel}},
	}
	if _, err := service.suspendCluster([]byte(`{"name":"demo"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("suspend did not cancel active provisioning")
	}
	if _, ok := service.provisions[item.Name]; ok {
		t.Fatal("suspend retained active provisioning entry")
	}
}

func TestPrepareSavedMachineClosesAfterSaveFailure(t *testing.T) {
	machine := &suspendMachineFake{suspendErr: errors.New("save failed")}

	retain, err := prepareSavedMachine(machine, "/tmp/node.vzstate")
	if !errors.Is(err, machine.suspendErr) {
		t.Fatalf("error = %v, want save failure", err)
	}
	if retain {
		t.Fatal("released machine must not remain tracked")
	}
	want := []string{"suspend /tmp/node.vzstate", "stop", "close"}
	if !reflect.DeepEqual(machine.calls, want) {
		t.Fatalf("calls = %v, want %v", machine.calls, want)
	}
}

func TestPrepareSavedMachineRetainsAfterStopFailure(t *testing.T) {
	machine := &suspendMachineFake{stopErr: errors.New("stop failed")}

	retain, err := prepareSavedMachine(machine, "/tmp/node.vzstate")
	if !errors.Is(err, machine.stopErr) {
		t.Fatalf("error = %v, want stop failure", err)
	}
	if !retain {
		t.Fatal("machine must remain tracked after an unconfirmed stop")
	}
	want := []string{"suspend /tmp/node.vzstate", "stop", "close"}
	if !reflect.DeepEqual(machine.calls, want) {
		t.Fatalf("calls = %v, want %v", machine.calls, want)
	}
}

func TestPrepareSavedMachineJoinsStopAndCloseFailures(t *testing.T) {
	machine := &suspendMachineFake{
		stopErr:  errors.New("stop failed"),
		closeErr: errors.New("close failed"),
	}

	retain, err := prepareSavedMachine(machine, "/tmp/node.vzstate")
	if !errors.Is(err, machine.stopErr) || !errors.Is(err, machine.closeErr) {
		t.Fatalf("error = %v, want joined stop and close failures", err)
	}
	if !retain {
		t.Fatal("machine must remain tracked after an unconfirmed stop")
	}
	want := []string{"suspend /tmp/node.vzstate", "stop", "close"}
	if !reflect.DeepEqual(machine.calls, want) {
		t.Fatalf("calls = %v, want %v", machine.calls, want)
	}
}

func TestPrepareSavedMachineRetainsMachineWhenCloseFails(t *testing.T) {
	machine := &suspendMachineFake{
		suspendErr: errors.New("save failed"),
		closeErr:   errors.New("close failed"),
	}

	retain, err := prepareSavedMachine(machine, "/tmp/node.vzstate")
	if !errors.Is(err, machine.suspendErr) || !errors.Is(err, machine.closeErr) {
		t.Fatalf("error = %v, want joined save and close failures", err)
	}
	if !retain {
		t.Fatal("machine must remain tracked when cleanup fails")
	}
}

func TestResumeNodeBatchCommitsSaveStatesOnlyAfterAllNodesResume(t *testing.T) {
	dir := t.TempDir()
	paths := map[string]string{
		"cp-1":     filepath.Join(dir, "cp-1.vzstate"),
		"worker-1": filepath.Join(dir, "worker-1.vzstate"),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("saved state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	rollbackCalled := false
	_, err := resumeNodeBatch(
		[]string{"cp-1", "worker-1"},
		func(node string) (resumedNode, error) {
			if node == "worker-1" {
				return resumedNode{}, errors.New("worker resume failed")
			}
			return resumedNode{savePath: paths[node]}, nil
		},
		func() error {
			rollbackCalled = true
			return nil
		},
	)
	if err == nil {
		t.Fatal("later node failure must fail the batch")
	}
	if !rollbackCalled {
		t.Fatal("later node failure did not trigger rollback")
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed resume consumed retryable save state %s: %v", path, err)
		}
	}

	_, err = resumeNodeBatch(
		[]string{"cp-1", "worker-1"},
		func(node string) (resumedNode, error) {
			return resumedNode{savePath: paths[node]}, nil
		},
		func() error { return errors.New("unexpected rollback") },
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("save state %s still exists or stat failed: %v", path, err)
		}
	}
}

func TestSuspendClusterFastFailsWithCapabilityReason(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("suspend-unsupported", 0, 1, 0, cluster.NodeDefaults{MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}

	machine := &suspendMachineFake{}
	service := &Server{
		hypervisor: &fakeHypervisor{
			capabilities: hypervisor.Capabilities{
				Suspend: hypervisor.FeatureStatus{
					Reason: "suspend requires QEMU >= 8.2 (found 6.2) — upgrade to Ubuntu 24.04+",
				},
			},
		},
		vms: map[string]map[string]hypervisor.Machine{
			item.Name: {item.Nodes[0].Name: machine},
		},
	}

	_, err = service.suspendCluster([]byte(`{"name":"suspend-unsupported"}`))
	if !errors.Is(err, hypervisor.ErrUnsupported) {
		t.Fatalf("suspendCluster() error = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "suspend requires QEMU >= 8.2 (found 6.2) — upgrade to Ubuntu 24.04+") {
		t.Fatalf("suspendCluster() error = %q, want capability reason", err)
	}
	if len(machine.calls) != 0 {
		t.Fatalf("suspendCluster() touched machine despite unsupported capability: %v", machine.calls)
	}
}

// TestSummaryReportsSuspensionFromSavedStateOnDisk pins the signal a restarted
// daemon can still see: tracked VMs are gone after a restart, so only the saved
// memory on disk tells a client the cluster is suspended rather than stopped.
func TestSummaryReportsSuspensionFromSavedStateOnDisk(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("suspended-signal", 0, 1, 0, cluster.NodeDefaults{MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}

	if summary(item, false).Suspended {
		t.Fatal("a stopped cluster without saved state must not report suspended")
	}

	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(saveStatePath(dir, item.Nodes[0].Name), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !summary(item, false).Suspended {
		t.Fatal("saved state on disk must report the cluster as suspended")
	}
	if summary(item, true).Suspended {
		t.Fatal("a running cluster is never suspended")
	}
}

func TestClockDriftWarning(t *testing.T) {
	now := time.Date(2026, 8, 21, 18, 48, 12, 0, time.UTC)

	for _, testCase := range []struct {
		name   string
		saves  []time.Time
		want   string
		absent bool
	}{
		{
			name:   "no saved state",
			absent: true,
		},
		{
			name:   "brief suspend stays quiet",
			saves:  []time.Time{now.Add(-2 * time.Second)},
			absent: true,
		},
		{
			name:  "drift reported from the oldest save",
			saves: []time.Time{now.Add(-30 * time.Second), now.Add(-2 * time.Minute)},
			want:  "2m0s behind",
		},
		{
			name:   "host clock moved backwards",
			saves:  []time.Time{now.Add(time.Minute)},
			absent: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			for i, modTime := range testCase.saves {
				path := saveStatePath(dir, fmt.Sprintf("node-%d", i))
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, modTime, modTime); err != nil {
					t.Fatal(err)
				}
			}
			warning := clockDriftWarning(dir, now)
			if testCase.absent {
				if warning != "" {
					t.Fatalf("warning = %q, want none", warning)
				}
				return
			}
			if !strings.Contains(warning, testCase.want) {
				t.Fatalf("warning = %q, want it to mention %q", warning, testCase.want)
			}
		})
	}
}
