package hostmem

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	SystemSnapshot = func(context.Context) (Snapshot, error) {
		return Snapshot{TotalMiB: 32768, AvailableMiB: 16384, Pressure: PressureNormal}, nil
	}
	os.Exit(m.Run())
}

func TestSystemSnapshotSeamIsHermetic(t *testing.T) {
	snapshot, err := SystemSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AvailableMiB != 16384 || snapshot.Pressure != PressureNormal {
		t.Fatalf("snapshot = %+v, want pinned roomy normal sample", snapshot)
	}
}

func TestUnsupportedSentinel(t *testing.T) {
	if !errors.Is(ErrUnsupported, ErrUnsupported) {
		t.Fatal("ErrUnsupported must be matchable with errors.Is")
	}
}
