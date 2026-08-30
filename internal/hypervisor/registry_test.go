package hypervisor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type registryTestHypervisor struct{ architecture Architecture }

func (h registryTestHypervisor) Launch(context.Context, Spec) (Machine, error) { return nil, nil }
func (h registryTestHypervisor) Capabilities() Capabilities                    { return Capabilities{} }
func (h registryTestHypervisor) Architecture() Architecture                    { return h.architecture }

func TestNewRegistryEagerlyRecordsUnavailableBackends(t *testing.T) {
	t.Parallel()

	var calls []Name
	sentinel := errors.New("optional probe failed")
	registry := newRegistry(context.Background(), Default{Name: "primary", Source: DefaultSourceCompiled}, []backendFactory{
		{name: "primary", new: func(context.Context) (Hypervisor, error) {
			calls = append(calls, "primary")
			return registryTestHypervisor{architecture: ArchitectureARM64}, nil
		}},
		{name: "optional", new: func(context.Context) (Hypervisor, error) {
			calls = append(calls, "optional")
			return nil, sentinel
		}},
	})

	if !reflect.DeepEqual(calls, []Name{"primary", "optional"}) {
		t.Fatalf("factory calls = %v, want both factories in declaration order", calls)
	}
	if _, backend, err := registry.ResolveDefault(); err != nil || backend == nil {
		t.Fatalf("ResolveDefault() = (_, %v, %v), want available backend", backend, err)
	}
	optional, ok := registry.Backends["optional"]
	if !ok || optional.Availability.Available {
		t.Fatalf("optional backend = %+v, want retained unavailable entry", optional)
	}
	if optional.Availability.Reason != sentinel.Error() {
		t.Fatalf("optional reason = %q, want %q", optional.Availability.Reason, sentinel)
	}
	_, err := registry.Resolve("optional")
	if !errors.Is(err, ErrUnsupported) || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "optional") {
		t.Fatalf("Resolve(optional) error = %v, want ErrUnsupported naming backend", err)
	}
}

func TestRegistryResolveDefaultRejectsUnavailableDefault(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("default probe failed")
	registry := newRegistry(context.Background(), Default{Name: "primary", Source: DefaultSourceCompiled}, []backendFactory{
		{name: "primary", new: func(context.Context) (Hypervisor, error) { return nil, sentinel }},
	})

	_, _, err := registry.ResolveDefault()
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), sentinel.Error()) {
		t.Fatalf("ResolveDefault() error = %v, want ErrUnsupported preserving %q", err, sentinel)
	}
}

func TestRegistryNamesAreSorted(t *testing.T) {
	t.Parallel()

	registry := Registry{Backends: map[Name]Backend{
		"zeta":  {},
		"alpha": {},
	}}
	if got, want := registry.Names(), []Name{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}
