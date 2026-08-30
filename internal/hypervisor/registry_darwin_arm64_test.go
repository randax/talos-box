//go:build darwin && arm64

package hypervisor

import (
	"context"
	"testing"
)

func TestPlatformRegistryIncludesCompiledDefaultFactory(t *testing.T) {
	t.Parallel()

	selection, factories := platformRegistrySpec()
	if selection.Name != NameVZ {
		t.Fatalf("default name = %q, want %q", selection.Name, NameVZ)
	}
	if selection.Source != DefaultSourceCompiled {
		t.Fatalf("default source = %q, want %q", selection.Source, DefaultSourceCompiled)
	}
	registry := newRegistry(context.Background(), selection, nil)
	if registry.CompiledDefault != registry.Default.Name {
		t.Fatalf("compiled default = %q, effective default = %q", registry.CompiledDefault, registry.Default.Name)
	}
	for _, factory := range factories {
		if factory.name == NameVZ {
			return
		}
	}
	t.Fatalf("default %q is absent from platform factories", selection.Name)
}

func TestDarwinARM64RegistryIncludesVZAndQEMU(t *testing.T) {
	t.Parallel()

	selection, factories := platformRegistrySpec()
	if selection != (Default{Name: NameVZ, Source: DefaultSourceCompiled}) {
		t.Fatalf("selection = %+v, want VZ compiled default", selection)
	}
	got := make(map[Name]bool, len(factories))
	for _, factory := range factories {
		got[factory.name] = true
	}
	if !got[NameVZ] || !got[NameQEMU] || len(got) != 2 {
		t.Fatalf("registered factories = %v, want VZ and QEMU", got)
	}
}

func TestDarwinARM64UnavailableQEMULeavesVZDefaultUsable(t *testing.T) {
	t.Parallel()

	vz := registryTestHypervisor{architecture: ArchitectureARM64}
	registry := newRegistry(context.Background(), Default{Name: NameVZ, Source: DefaultSourceCompiled}, []backendFactory{
		{name: NameVZ, new: func(context.Context) (Hypervisor, error) { return vz, nil }},
		{name: NameQEMU, new: func(context.Context) (Hypervisor, error) {
			return nil, newUnavailableError("QEMU unavailable", "install QEMU", nil)
		}},
	})
	name, backend, err := registry.ResolveDefault()
	if err != nil {
		t.Fatal(err)
	}
	if name != NameVZ || backend != vz {
		t.Fatalf("default = %q %T, want usable VZ", name, backend)
	}
	if registry.Backends[NameQEMU].Availability.Available {
		t.Fatal("unavailable QEMU was marked available")
	}
}
