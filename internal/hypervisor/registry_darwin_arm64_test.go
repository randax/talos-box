//go:build darwin && arm64

package hypervisor

import "testing"

func TestPlatformRegistryIncludesCompiledDefaultFactory(t *testing.T) {
	t.Parallel()

	selection, factories := platformRegistrySpec()
	if selection.Source != DefaultSourceCompiled {
		t.Fatalf("default source = %q, want %q", selection.Source, DefaultSourceCompiled)
	}
	for _, factory := range factories {
		if factory.name == selection.Name {
			return
		}
	}
	t.Fatalf("default %q is absent from platform factories", selection.Name)
}
