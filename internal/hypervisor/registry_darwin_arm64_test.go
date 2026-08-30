//go:build darwin && arm64

package hypervisor

import "testing"

func TestPlatformRegistryIncludesCompiledDefaultFactory(t *testing.T) {
	t.Parallel()

	selection, factories := platformRegistrySpec()
	if selection.Name != NameVZ {
		t.Fatalf("default name = %q, want %q", selection.Name, NameVZ)
	}
	if selection.Source != DefaultSourceCompiled {
		t.Fatalf("default source = %q, want %q", selection.Source, DefaultSourceCompiled)
	}
	for _, factory := range factories {
		if factory.name == NameVZ {
			return
		}
	}
	t.Fatalf("default %q is absent from platform factories", selection.Name)
}
