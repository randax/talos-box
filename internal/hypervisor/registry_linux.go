//go:build linux

package hypervisor

import "context"

func platformRegistrySpec() (Default, []backendFactory) {
	return Default{Name: NameQEMU, Source: DefaultSourceCompiled}, []backendFactory{{name: NameQEMU, new: newQEMU}}
}

// NewAll probes every backend compiled for this platform.
func NewAll(ctx context.Context) Registry {
	selection, factories := platformRegistrySpec()
	return newRegistry(ctx, selection, factories)
}
