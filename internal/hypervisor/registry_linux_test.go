//go:build linux

package hypervisor

import "testing"

func TestPlatformRegistryIncludesCompiledDefaultFactory(t *testing.T) {
	t.Parallel()

	selection, factories := platformRegistrySpec()
	if selection.Name != NameQEMU {
		t.Fatalf("default name = %q, want %q", selection.Name, NameQEMU)
	}
	if selection.Source != DefaultSourceCompiled {
		t.Fatalf("default source = %q, want %q", selection.Source, DefaultSourceCompiled)
	}
	for _, factory := range factories {
		if factory.name == NameQEMU {
			return
		}
	}
	t.Fatalf("default %q is absent from platform factories", selection.Name)
}
