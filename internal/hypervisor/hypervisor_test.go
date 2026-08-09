package hypervisor

import (
	"context"
	"errors"
	"testing"
)

type contractMachine struct{}

func (contractMachine) Active() bool                          { return false }
func (contractMachine) SetMemoryTargetMiB(int) error          { return nil }
func (contractMachine) Stop(context.Context) error            { return nil }
func (contractMachine) Suspend(context.Context, string) error { return nil }
func (contractMachine) Close() error                          { return nil }

func TestContractSentinelErrorsAreDistinct(t *testing.T) {
	t.Parallel()
	if errors.Is(ErrUnsupported, ErrDeviceNotActive) ||
		errors.Is(ErrUnsupported, ErrIncompatibleSave) ||
		errors.Is(ErrDeviceNotActive, ErrIncompatibleSave) {
		t.Fatal("hypervisor sentinel errors must remain distinct")
	}
}

func TestMachineContract(t *testing.T) {
	t.Parallel()
	var _ Machine = contractMachine{}
}

func TestNewHonorsCanceledProbeContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("New(canceled context) = %v, want context.Canceled", err)
	}
}
