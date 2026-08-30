//go:build darwin && amd64

package hypervisor

import "testing"

func TestDarwinAMD64RegistryUsesQEMUDefault(t *testing.T) {
	t.Parallel()

	selection, factories := platformRegistrySpec()
	if selection != (Default{Name: NameQEMU, Source: DefaultSourceCompiled}) {
		t.Fatalf("selection = %+v, want QEMU compiled default", selection)
	}
	if len(factories) != 1 || factories[0].name != NameQEMU {
		t.Fatalf("factories = %+v, want QEMU alone", factories)
	}
}
