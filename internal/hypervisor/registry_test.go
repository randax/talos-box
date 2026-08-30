package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type registryTestHypervisor struct{ architecture Architecture }

func (h registryTestHypervisor) Launch(context.Context, Spec) (Machine, error) { return nil, nil }
func (h registryTestHypervisor) Capabilities() Capabilities                    { return Capabilities{} }
func (h registryTestHypervisor) Architecture() Architecture                    { return h.architecture }

func TestParseNameAcceptsKnownHypervisors(t *testing.T) {
	t.Parallel()

	for _, want := range []Name{NameVZ, NameQEMU} {
		got, err := ParseName(string(want))
		if err != nil {
			t.Fatalf("ParseName(%q): %v", want, err)
		}
		if got != want {
			t.Fatalf("ParseName(%q) = %q, want %q", want, got, want)
		}
	}
}

func TestParseNameRejectsUnknownHypervisor(t *testing.T) {
	t.Parallel()

	_, err := ParseName("xen")
	if got, want := err.Error(), `hypervisor must be one of vz | qemu (got "xen")`; got != want {
		t.Fatalf("ParseName() error = %q, want %q", got, want)
	}
}

func TestRegistryWithDefaultRecordsEnvironmentSource(t *testing.T) {
	t.Parallel()

	registry := Registry{
		Backends:        map[Name]Backend{NameVZ: {}, NameQEMU: {}},
		Default:         Default{Name: NameVZ, Source: DefaultSourceCompiled},
		CompiledDefault: NameVZ,
	}
	got, err := registry.WithDefault(string(NameQEMU), DefaultSourceEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if got.Default != (Default{Name: NameQEMU, Source: DefaultSourceEnvironment}) {
		t.Fatalf("default = %+v, want environment-selected %q", got.Default, NameQEMU)
	}
	if got.CompiledDefault != NameVZ {
		t.Fatalf("compiled default = %q, want immutable %q", got.CompiledDefault, NameVZ)
	}
}

func TestRegistryWithDefaultAllowsUnavailableRegisteredBackend(t *testing.T) {
	t.Parallel()

	registry := Registry{
		Backends:        map[Name]Backend{NameVZ: {Availability: Availability{Available: true}}, NameQEMU: {}},
		Default:         Default{Name: NameVZ, Source: DefaultSourceCompiled},
		CompiledDefault: NameVZ,
	}
	got, err := registry.WithDefault(string(NameQEMU), DefaultSourceEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if got.Default.Name != NameQEMU {
		t.Fatalf("default name = %q, want unavailable registered backend %q", got.Default.Name, NameQEMU)
	}
}

func TestRegistryWithDefaultRejectsUnregisteredBackend(t *testing.T) {
	t.Parallel()

	registry := Registry{Backends: map[Name]Backend{NameVZ: {}}, CompiledDefault: NameVZ}
	_, err := registry.WithDefault(string(NameQEMU), DefaultSourceEnvironment)
	if err == nil || !strings.Contains(err.Error(), `hypervisor "qemu" is not registered`) {
		t.Fatalf("WithDefault() error = %v, want unregistered backend refusal", err)
	}
}

func TestRegistryDefaultFromEnvironmentRejectsUnknownHypervisor(t *testing.T) {
	t.Parallel()

	registry := Registry{Backends: map[Name]Backend{NameVZ: {}}, Default: Default{Name: NameVZ, Source: DefaultSourceCompiled}, CompiledDefault: NameVZ}
	_, err := withDefaultFromEnvironment(registry, func(string) (string, bool) { return "xen", true })
	if got, want := err.Error(), `TBX_HYPERVISOR: hypervisor must be one of vz | qemu (got "xen")`; got != want {
		t.Fatalf("environment selection error = %q, want %q", got, want)
	}
}

func TestRegistryDefaultFromEnvironmentIgnoresUnsetAndEmpty(t *testing.T) {
	t.Parallel()

	want := Registry{Backends: map[Name]Backend{NameVZ: {}}, Default: Default{Name: NameVZ, Source: DefaultSourceCompiled}, CompiledDefault: NameVZ}
	for _, lookup := range []func(string) (string, bool){
		func(string) (string, bool) { return "", false },
		func(string) (string, bool) { return "", true },
	} {
		got, err := withDefaultFromEnvironment(want, lookup)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("empty environment changed registry: %+v", got)
		}
	}
}

func TestNewRegistryEagerlyRecordsUnavailableBackends(t *testing.T) {
	t.Parallel()

	var calls []Name
	sentinel := errors.New("optional probe failed")
	const remediation = "install the optional backend"
	registry := newRegistry(context.Background(), Default{Name: "primary", Source: DefaultSourceCompiled}, []backendFactory{
		{name: "primary", new: func(context.Context) (Hypervisor, error) {
			calls = append(calls, "primary")
			return registryTestHypervisor{architecture: ArchitectureARM64}, nil
		}},
		{name: "optional", new: func(context.Context) (Hypervisor, error) {
			calls = append(calls, "optional")
			return nil, newUnavailableError(sentinel.Error(), remediation, sentinel)
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
	if optional.Availability.Remediation != remediation {
		t.Fatalf("optional remediation = %q, want %q", optional.Availability.Remediation, remediation)
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

func TestRegistryResolveIncludesUnavailableReasonAndWrappedError(t *testing.T) {
	t.Parallel()

	inner := errors.New("exit status 1")
	registry := Registry{Backends: map[Name]Backend{
		NameQEMU: {Availability: Availability{
			Reason: "QEMU does not list the hvf accelerator",
			Err:    inner,
		}},
	}}

	_, err := registry.Resolve(NameQEMU)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Resolve(qemu) error = %v, want ErrUnsupported", err)
	}
	if !errors.Is(err, inner) {
		t.Fatalf("Resolve(qemu) error = %v, want wrapped %v", err, inner)
	}
	if !strings.Contains(err.Error(), "QEMU does not list the hvf accelerator") {
		t.Fatalf("Resolve(qemu) error = %q, want availability reason", err)
	}
}

func TestRegistryResolveAvoidsDuplicatingReasonWhenWrappedErrorMatches(t *testing.T) {
	t.Parallel()

	inner := errors.New("exit status 1")
	registry := Registry{Backends: map[Name]Backend{
		NameQEMU: {Availability: Availability{
			Reason: inner.Error(),
			Err:    inner,
		}},
	}}

	_, err := registry.Resolve(NameQEMU)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Resolve(qemu) error = %v, want ErrUnsupported", err)
	}
	if !errors.Is(err, inner) {
		t.Fatalf("Resolve(qemu) error = %v, want wrapped %v", err, inner)
	}
	if strings.Count(err.Error(), inner.Error()) != 1 {
		t.Fatalf("Resolve(qemu) error = %q, expected inner message once", err)
	}
}

func TestRegistryResolveAvoidsDuplicatingReasonWhenWrappedErrorAlreadyWrapsUnsupported(t *testing.T) {
	t.Parallel()

	const reason = "no matching EFI firmware pair found for arm64"
	tests := []struct {
		name  string
		inner error
	}{
		{
			name:  "direct unsupported wrap",
			inner: fmt.Errorf("%w: %s", ErrUnsupported, reason),
		},
		{
			name:  "nested unsupported wrap",
			inner: fmt.Errorf("discover firmware: %w", fmt.Errorf("%w: %s", ErrUnsupported, reason)),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			registry := Registry{Backends: map[Name]Backend{
				NameQEMU: {Availability: Availability{Reason: reason, Err: test.inner}},
			}}

			_, err := registry.Resolve(NameQEMU)
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("Resolve(qemu) error = %v, want ErrUnsupported", err)
			}
			if !errors.Is(err, test.inner) {
				t.Fatalf("Resolve(qemu) error = %v, want wrapped %v", err, test.inner)
			}
			if got, want := err.Error(), `hypervisor feature unsupported: hypervisor "qemu": `+reason; got != want {
				t.Fatalf("Resolve(qemu) error = %q, want %q", got, want)
			}
			unwraps, ok := err.(interface{ Unwrap() []error })
			if !ok {
				t.Fatalf("Resolve(qemu) error %T does not expose multi-unwrapping", err)
			}
			if got := unwraps.Unwrap(); len(got) != 2 || got[0] != ErrUnsupported || got[1] != test.inner {
				t.Fatalf("Resolve(qemu) unwrap = %#v, want [ErrUnsupported inner]", got)
			}
		})
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
