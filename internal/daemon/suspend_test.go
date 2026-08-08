package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/randax/talos-box/internal/vm"
)

type suspendMachineFake struct {
	calls      []string
	suspendErr error
	stopErr    error
	closeErr   error
}

func (f *suspendMachineFake) Suspend(path string) error {
	f.calls = append(f.calls, "suspend "+path)
	return f.suspendErr
}

func (f *suspendMachineFake) StopAfterSave() error {
	f.calls = append(f.calls, "stop-after-save")
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

	want := []string{"suspend /tmp/node.vzstate", "stop-after-save"}
	if !reflect.DeepEqual(machine.calls, want) {
		t.Fatalf("calls = %v, want %v", machine.calls, want)
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
	want := []string{"suspend /tmp/node.vzstate", "close"}
	if !reflect.DeepEqual(machine.calls, want) {
		t.Fatalf("calls = %v, want %v", machine.calls, want)
	}
}

func TestPrepareSavedMachineClosesAfterStopFailure(t *testing.T) {
	machine := &suspendMachineFake{stopErr: errors.New("stop failed")}

	retain, err := prepareSavedMachine(machine, "/tmp/node.vzstate")
	if !errors.Is(err, machine.stopErr) {
		t.Fatalf("error = %v, want stop failure", err)
	}
	if retain {
		t.Fatal("released machine must not remain tracked")
	}
	want := []string{"suspend /tmp/node.vzstate", "stop-after-save", "close"}
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

func TestMachineForResumeReusesRetainedInstance(t *testing.T) {
	retained := &vm.VM{}
	created := false

	got, err := machineForResume(retained, func() (*vm.VM, error) {
		created = true
		return &vm.VM{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != retained {
		t.Fatal("resume did not reuse the retained VM")
	}
	if created {
		t.Fatal("resume recreated the VM despite retained device state")
	}
}

func TestMachineForResumeCreatesInstanceWithoutRetainedState(t *testing.T) {
	created := &vm.VM{}

	got, err := machineForResume(nil, func() (*vm.VM, error) { return created, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got != created {
		t.Fatal("resume did not return the freshly-created VM")
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

func TestResumeNodeRestoresWhenSaveValid(t *testing.T) {
	var restored, coldBooted bool
	warning, err := resumeNode(true,
		func() error { restored = true; return nil },
		func() error { coldBooted = true; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !restored || coldBooted {
		t.Errorf("valid save: restored=%v coldBooted=%v, want restore only", restored, coldBooted)
	}
	if warning != "" {
		t.Errorf("valid restore should not warn, got %q", warning)
	}
}

func TestResumeNodeColdBootsWhenSaveMissing(t *testing.T) {
	var restored, coldBooted bool
	warning, err := resumeNode(false,
		func() error { restored = true; return nil },
		func() error { coldBooted = true; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored || !coldBooted {
		t.Errorf("missing save: restored=%v coldBooted=%v, want cold boot only", restored, coldBooted)
	}
	if warning == "" {
		t.Error("missing save should produce a warning")
	}
}

func TestResumeNodeColdBootsWhenRestoreFails(t *testing.T) {
	var coldBooted bool
	warning, err := resumeNode(true,
		func() error { return errors.New("incompatible saved state") },
		func() error { coldBooted = true; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !coldBooted {
		t.Error("failed restore must fall back to cold boot")
	}
	if warning == "" {
		t.Error("failed restore should produce a warning")
	}
}

func TestResumeNodePropagatesColdBootFailure(t *testing.T) {
	_, err := resumeNode(false,
		func() error { return nil },
		func() error { return errors.New("no image") },
	)
	if err == nil {
		t.Fatal("cold-boot failure must surface (nothing else to fall back to)")
	}
}
