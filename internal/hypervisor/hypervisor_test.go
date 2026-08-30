package hypervisor

import (
	"context"
	"errors"
	"strings"
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
	_, _, err := NewAll(ctx).ResolveDefault()
	if !errors.Is(err, ErrUnsupported) || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("NewAll(canceled context) default error = %v, want gated backend preserving cancellation", err)
	}
}
