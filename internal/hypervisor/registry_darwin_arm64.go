//go:build darwin && arm64

package hypervisor

import "context"

func platformRegistrySpec() (Default, []backendFactory) {
	return Default{Name: NameVZ, Source: DefaultSourceCompiled}, []backendFactory{{name: NameVZ, new: newVZ}}
}

// NewAll probes every backend compiled for this platform.
func NewAll(ctx context.Context) Registry {
	selection, factories := platformRegistrySpec()
	return newRegistry(ctx, selection, factories)
}
