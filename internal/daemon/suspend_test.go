package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
