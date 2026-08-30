//go:build linux

package hypervisor

import (
	"context"
	"testing"
)

func TestPlatformRegistryIncludesCompiledDefaultFactory(t *testing.T) {
	t.Parallel()

	selection, factories := platformRegistrySpec()
	if selection.Name != NameQEMU {
		t.Fatalf("default name = %q, want %q", selection.Name, NameQEMU)
	}
	if selection.Source != DefaultSourceCompiled {
		t.Fatalf("default source = %q, want %q", selection.Source, DefaultSourceCompiled)
	}
	registry := newRegistry(context.Background(), selection, nil)
	if registry.CompiledDefault != registry.Default.Name {
		t.Fatalf("compiled default = %q, effective default = %q", registry.CompiledDefault, registry.Default.Name)
	}
	for _, factory := range factories {
		if factory.name == NameQEMU {
			return
		}
	}
	t.Fatalf("default %q is absent from platform factories", selection.Name)
}
