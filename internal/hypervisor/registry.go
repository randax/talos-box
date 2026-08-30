package hypervisor

import (
	"context"
	"fmt"
	"sort"
)

// Name identifies a hypervisor backend.
type Name string

const (
	// NameVZ identifies the Virtualization.framework backend.
	NameVZ Name = "vz"
	// NameQEMU identifies the QEMU backend.
	NameQEMU Name = "qemu"
)

// DefaultSource identifies how the active default was selected.
type DefaultSource string

// DefaultSourceCompiled is the platform's built-in selection.
const DefaultSourceCompiled DefaultSource = "compiled"

// Default records the selected backend and the source of that selection.
type Default struct {
	Name   Name
	Source DefaultSource
}

// Availability records whether a probed backend can be used.
type Availability struct {
	Available bool
	Reason    string
}

// Backend pairs a successful probe with its availability gate.
type Backend struct {
	Hypervisor   Hypervisor
	Availability Availability
}

// Registry contains every known backend and the current default.
type Registry struct {
	Backends map[Name]Backend
	Default  Default
}

// Names returns registered backend names in lexical order.
func (r Registry) Names() []Name {
	names := make([]Name, 0, len(r.Backends))
	for name := range r.Backends {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// Resolve returns an available named backend.
func (r Registry) Resolve(name Name) (Hypervisor, error) {
	backend, ok := r.Backends[name]
	if !ok {
		return nil, fmt.Errorf("%w: hypervisor %q is not registered", ErrUnsupported, name)
	}
	if !backend.Availability.Available || backend.Hypervisor == nil {
		reason := backend.Availability.Reason
		if reason == "" {
			reason = "backend is unavailable"
		}
		return nil, fmt.Errorf("%w: hypervisor %q: %s", ErrUnsupported, name, reason)
	}
	return backend.Hypervisor, nil
}

// ResolveDefault returns the selected backend when its probe succeeded.
func (r Registry) ResolveDefault() (Name, Hypervisor, error) {
	backend, err := r.Resolve(r.Default.Name)
	return r.Default.Name, backend, err
}

type backendFactory struct {
	name Name
	new  func(context.Context) (Hypervisor, error)
}

func newRegistry(ctx context.Context, selection Default, factories []backendFactory) Registry {
	registry := Registry{Backends: make(map[Name]Backend, len(factories)), Default: selection}
	for _, factory := range factories {
		backend, err := factory.new(ctx)
		if err != nil {
			registry.Backends[factory.name] = Backend{Availability: Availability{Reason: err.Error()}}
			continue
		}
		registry.Backends[factory.name] = Backend{
			Hypervisor:   backend,
			Availability: Availability{Available: true},
		}
	}
	return registry
}
