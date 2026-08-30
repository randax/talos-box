package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// ParseName validates a configured hypervisor name.
func ParseName(raw string) (Name, error) {
	name := Name(raw)
	switch name {
	case NameVZ, NameQEMU:
		return name, nil
	default:
		return "", fmt.Errorf("hypervisor must be one of vz | qemu (got %q)", raw)
	}
}

// DefaultSource identifies how the active default was selected.
type DefaultSource string

const (
	// DefaultEnv selects the daemon's default hypervisor.
	DefaultEnv = "TBX_HYPERVISOR"

	// DefaultSourceCompiled is the platform's built-in selection.
	DefaultSourceCompiled DefaultSource = "compiled"
	// DefaultSourceEnvironment records a selection from DefaultEnv.
	DefaultSourceEnvironment DefaultSource = DefaultEnv
)

// Default records the selected backend and the source of that selection.
type Default struct {
	Name   Name
	Source DefaultSource
}

// Availability records whether a probed backend can be used.
type Availability struct {
	Available   bool
	Reason      string
	Remediation string
	Err         error
}

// Backend pairs a successful probe with its availability gate.
type Backend struct {
	Hypervisor   Hypervisor
	Availability Availability
}

// Registry contains every known backend and the current default.
type Registry struct {
	Backends        map[Name]Backend
	Default         Default
	CompiledDefault Name
}

// WithDefault returns a copy with a different effective default.
func (r Registry) WithDefault(raw string, source DefaultSource) (Registry, error) {
	name, err := ParseName(raw)
	if err != nil {
		return Registry{}, err
	}
	if _, ok := r.Backends[name]; !ok {
		return Registry{}, fmt.Errorf("hypervisor %q is not registered", name)
	}
	r.Default = Default{Name: name, Source: source}
	return r, nil
}

// NewAllFromEnvironment probes all platform backends and applies DefaultEnv.
func NewAllFromEnvironment(ctx context.Context) (Registry, error) {
	return withDefaultFromEnvironment(NewAll(ctx), os.LookupEnv)
}

func withDefaultFromEnvironment(registry Registry, lookupEnv func(string) (string, bool)) (Registry, error) {
	raw, ok := lookupEnv(DefaultEnv)
	if !ok || raw == "" {
		return registry, nil
	}
	configured, err := registry.WithDefault(raw, DefaultSourceEnvironment)
	if err != nil {
		return Registry{}, fmt.Errorf("%s: %w", DefaultEnv, err)
	}
	return configured, nil
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
		if backend.Availability.Err != nil {
			if reason != backend.Availability.Err.Error() {
				return nil, fmt.Errorf("%w: hypervisor %q: %s: %w", ErrUnsupported, name, reason, backend.Availability.Err)
			}
			return nil, fmt.Errorf("%w: hypervisor %q: %w", ErrUnsupported, name, backend.Availability.Err)
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

type unavailableError struct {
	reason      string
	remediation string
	err         error
}

func newUnavailableError(reason, remediation string, err error) error {
	return unavailableError{reason: reason, remediation: remediation, err: err}
}

func (e unavailableError) Error() string { return e.reason }
func (e unavailableError) Unwrap() error { return e.err }

func newRegistry(ctx context.Context, selection Default, factories []backendFactory) Registry {
	registry := Registry{
		Backends:        make(map[Name]Backend, len(factories)),
		Default:         selection,
		CompiledDefault: selection.Name,
	}
	for _, factory := range factories {
		backend, err := factory.new(ctx)
		if err != nil {
			var unavailable unavailableError
			if errors.As(err, &unavailable) {
				registry.Backends[factory.name] = Backend{Availability: Availability{
					Reason: unavailable.reason, Remediation: unavailable.remediation, Err: unavailable.err,
				}}
				continue
			}
			registry.Backends[factory.name] = Backend{Availability: Availability{Reason: err.Error(), Err: err}}
			continue
		}
		registry.Backends[factory.name] = Backend{
			Hypervisor:   backend,
			Availability: Availability{Available: true},
		}
	}
	return registry
}
