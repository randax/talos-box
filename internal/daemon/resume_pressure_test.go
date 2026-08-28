package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// nappingMemoryMiB is the suspended cluster's whole configured footprint: a
// resume re-admits every byte of it at once, which is exactly what the
// projected-start gate charges for.
const nappingMemoryMiB = 2048

// suspendedClusterForResume builds the shape #368 is about: one cluster the
// daemon is not running (the resume target) beside another whose guests are
// resident, so the projected-start gate has memory already spoken for. Every
// host reading is the caller's to pin.
func suspendedClusterForResume(t *testing.T) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	napping, err := cluster.New("napping", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: nappingMemoryMiB, DiskGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(napping); err != nil {
		t.Fatal(err)
	}
	busy, err := cluster.New("busy", 1, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024, DiskGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(busy); err != nil {
		t.Fatal(err)
	}

	backend := &fakeHypervisor{launch: func(context.Context, hypervisor.Spec) (hypervisor.Machine, error) {
		return &fakeMachine{active: true}, nil
	}}
	resident := make(map[string]hypervisor.Machine, len(busy.Nodes))
	for _, node := range busy.Nodes {
		resident[node.Name] = &fakeMachine{active: true}
	}
	return &Server{
		hypervisor:    backend,
		vms:           map[string]map[string]hypervisor.Machine{busy.Name: resident},
		subnetSources: emptySubnetSources(),
	}
}

// TestResumeGatesOnHostPressureWithNoOtherGuestsResident pins the half of #368
// the projected-start gate cannot cover: that gate stands down when no other
// guest is resident, and a resume target is by definition not running. A lone
// suspended cluster on a host `cluster start` would refuse must be refused too,
// which is what the overcommit/host-pressure pair beside it is for.
func TestResumeGatesOnHostPressureWithNoOtherGuestsResident(t *testing.T) {
	for _, test := range []struct {
		name  string
		force bool
	}{
		{name: "refused under blocking host pressure"},
		{name: "forced past the refusal with the finding kept as a warning", force: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			napping, err := cluster.New("napping", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: nappingMemoryMiB, DiskGiB: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := cluster.Save(napping); err != nil {
				t.Fatal(err)
			}
			service := &Server{
				hypervisor: &fakeHypervisor{launch: func(context.Context, hypervisor.Spec) (hypervisor.Machine, error) {
					return &fakeMachine{active: true}, nil
				}},
				vms:             map[string]map[string]hypervisor.Machine{},
				subnetSources:   emptySubnetSources(),
				hostPressure:    extremeSwapPressure,
				hostTotalMemory: plentifulHostMemory,
				hostFreeMemory:  scarceHostMemory,
			}

			raw, err := json.Marshal(startArgs{Name: "napping", Force: test.force})
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.resumeCluster(raw)

			if !test.force {
				if err == nil || !strings.Contains(err.Error(), "host swap is 90% used") {
					t.Fatalf("resumeCluster() error = %v, want the host-pressure refusal", err)
				}
				if _, running := service.vms["napping"]; running {
					t.Fatal("resumeCluster() started guests despite the refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("forced resumeCluster() error = %v, want admission", err)
			}
			joined := strings.Join(result.Warnings, "\n")
			if !strings.Contains(joined, "host swap is 90% used") || !strings.Contains(joined, "(forced)") {
				t.Fatalf("forced resumeCluster() warnings = %q, want the forced host-pressure finding", result.Warnings)
			}
		})
	}
}

// TestResumeAnswersToTheProvisionStartGate pins both halves of #368: a resume
// commits the suspended cluster's whole footprint on a host that may already be
// full, so it must refuse under pressure like create/start do — and --force
// must be the same override there as everywhere else.
func TestResumeAnswersToTheProvisionStartGate(t *testing.T) {
	reserve := balloon.DefaultConfig().ReserveMiB
	tests := []struct {
		name        string
		freeMiB     int
		force       bool
		wantErr     string
		wantWarning string
	}{
		{
			name:    "refused without headroom for the restored footprint",
			freeMiB: nappingMemoryMiB + reserve - 1,
			wantErr: "guests are already running",
		},
		{
			name:        "forced past the refusal with the finding kept as a warning",
			freeMiB:     nappingMemoryMiB + reserve - 1,
			force:       true,
			wantWarning: "guests are already running",
		},
		{
			name:    "admitted with headroom to spare",
			freeMiB: nappingMemoryMiB + reserve,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := suspendedClusterForResume(t)
			service.hostPressure = noHostPressure
			service.hostTotalMemory = plentifulHostMemory
			service.hostFreeMemory = func() (int, error) { return test.freeMiB, nil }

			raw, err := json.Marshal(startArgs{Name: "napping", Force: test.force})
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.resumeCluster(raw)

			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("resumeCluster() error = %v, want the projected-start refusal", err)
				}
				if _, running := service.vms["napping"]; running {
					t.Fatal("resumeCluster() started guests despite the refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("resumeCluster() error = %v, want admission", err)
			}
			joined := strings.Join(result.Warnings, "\n")
			if test.wantWarning == "" {
				if strings.Contains(joined, "guests are already running") {
					t.Fatalf("resumeCluster() warnings = %q, want no pressure finding", result.Warnings)
				}
				return
			}
			if !strings.Contains(joined, test.wantWarning) || !strings.Contains(joined, "(forced)") {
				t.Fatalf("forced resumeCluster() warnings = %q, want the forced projected-start finding", result.Warnings)
			}
		})
	}
}
