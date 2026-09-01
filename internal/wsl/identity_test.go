package wsl

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type stubWindowsInterop struct {
	build string
	err   error
	calls int
}

func (s *stubWindowsInterop) WindowsBuild() (string, error) {
	s.calls++
	return s.build, s.err
}

func TestGenerationFromOSRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		release string
		want    Generation
	}{
		{name: "native Linux", release: "6.8.0-45-generic", want: NotWSL},
		{name: "mixed-case WSL1", release: "4.4.0-MiCrOsOfT-standard", want: WSL1},
		{name: "canonical WSL2", release: "5.15.167.4-microsoft-standard-WSL2", want: WSL2},
		{name: "mixed-case WSL2", release: "5.15.167.4-Microsoft-standard-wSl2", want: WSL2},
		{name: "trailing newline", release: "5.15.167.4-microsoft-standard-WSL2\n", want: WSL2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := GenerationFromOSRelease(tt.release); got != tt.want {
				t.Fatalf("GenerationFromOSRelease(%q) = %v, want %v", tt.release, got, tt.want)
			}
		})
	}
}

func TestDetectNativeLinuxStopsBeforeWSLAndWindowsProbes(t *testing.T) {
	t.Parallel()

	fail := func(name string) { t.Fatalf("%s called for native Linux", name) }
	windows := &stubWindowsInterop{}
	identity := Detect(Detector{
		ReadFile: func(string) ([]byte, error) { return []byte("6.8.0-45-generic\n"), nil },
		Command: func(string, ...string) ([]byte, error) {
			fail("Command")
			return nil, nil
		},
		LookupEnv: func(string) (string, bool) {
			fail("LookupEnv")
			return "", false
		},
		NATPrefix: func() (string, error) {
			fail("NATPrefix")
			return "", nil
		},
		Windows: windows,
	})
	if identity.Generation != NotWSL || identity.IsWSL() || identity.IsWSL2() {
		t.Fatalf("identity = %+v, want native Linux", identity)
	}
	if windows.calls != 0 {
		t.Fatalf("WindowsInterop calls = %d, want 0", windows.calls)
	}
}

func TestDetectWSL2CollectsEveryIdentityFieldOnce(t *testing.T) {
	t.Parallel()

	var commands []string
	natCalls := 0
	windows := &stubWindowsInterop{build: "26100"}
	identity := Detect(Detector{
		ReadFile: func(string) ([]byte, error) { return []byte("5.15.167.4-microsoft-standard-WSL2\n"), nil },
		Command: func(name string, args ...string) ([]byte, error) {
			commands = append(commands, name+" "+fmt.Sprint(args))
			switch fmt.Sprint(args) {
			case "[--version]":
				return []byte(" 2.7.12\n"), nil
			case "[--networking-mode]":
				return []byte(" NAT\n"), nil
			default:
				return nil, fmt.Errorf("unexpected command %s %v", name, args)
			}
		},
		LookupEnv: func(name string) (string, bool) {
			if name != "WSL_DISTRO_NAME" {
				t.Fatalf("LookupEnv(%q)", name)
			}
			return "Ubuntu-24.04", true
		},
		NATPrefix: func() (string, error) {
			natCalls++
			return "172.19.144.0/20", nil
		},
		Windows: windows,
	})
	if !identity.IsWSL() || !identity.IsWSL2() || identity.KernelRelease != "5.15.167.4-microsoft-standard-WSL2" {
		t.Fatalf("identity = %+v", identity)
	}
	wantValues := []string{identity.WSLVersion.Value, identity.Distribution.Value, identity.WindowsBuild.Value, identity.NetworkingMode.Value, identity.NATPrefix.Value}
	if want := []string{"2.7.12", "Ubuntu-24.04", "26100", "nat", "172.19.144.0/20"}; !reflect.DeepEqual(wantValues, want) {
		t.Fatalf("values = %v, want %v", wantValues, want)
	}
	if want := []string{"wslinfo [--version]", "wslinfo [--networking-mode]"}; !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %v, want %v", commands, want)
	}
	if windows.calls != 1 || natCalls != 1 {
		t.Fatalf("Windows calls = %d, NAT calls = %d, want 1 each", windows.calls, natCalls)
	}
}

func TestDetectWSL2DegradesObservationsIndependently(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		version    []byte
		versionErr error
		distro     string
		distroOK   bool
		buildErr   error
		mode       []byte
		modeErr    error
		natErr     error
		failed     func(Identity) Observation
	}{
		{name: "version error", versionErr: errors.New("version"), distro: "Ubuntu-24.04", distroOK: true, mode: []byte("nat"), failed: func(i Identity) Observation { return i.WSLVersion }},
		{name: "empty version", version: []byte(" \n"), distro: "Ubuntu-24.04", distroOK: true, mode: []byte("nat"), failed: func(i Identity) Observation { return i.WSLVersion }},
		{name: "missing distro", version: []byte("2.7.12"), mode: []byte("nat"), failed: func(i Identity) Observation { return i.Distribution }},
		{name: "Windows error", version: []byte("2.7.12"), distro: "Ubuntu-24.04", distroOK: true, buildErr: errors.New("interop"), mode: []byte("nat"), failed: func(i Identity) Observation { return i.WindowsBuild }},
		{name: "networking error", version: []byte("2.7.12"), distro: "Ubuntu-24.04", distroOK: true, modeErr: errors.New("mode"), failed: func(i Identity) Observation { return i.NetworkingMode }},
		{name: "empty networking", version: []byte("2.7.12"), distro: "Ubuntu-24.04", distroOK: true, mode: []byte(" \n"), failed: func(i Identity) Observation { return i.NetworkingMode }},
		{name: "NAT error", version: []byte("2.7.12"), distro: "Ubuntu-24.04", distroOK: true, mode: []byte("nat"), natErr: errors.New("route"), failed: func(i Identity) Observation { return i.NATPrefix }},
		{name: "combined errors", versionErr: errors.New("version"), buildErr: errors.New("interop"), modeErr: errors.New("mode"), natErr: errors.New("route"), failed: func(i Identity) Observation { return i.WindowsBuild }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			identity := Detect(Detector{
				ReadFile: func(string) ([]byte, error) { return []byte("5.15.167.4-microsoft-standard-WSL2"), nil },
				Command: func(_ string, args ...string) ([]byte, error) {
					if args[0] == "--version" {
						return tt.version, tt.versionErr
					}
					return tt.mode, tt.modeErr
				},
				LookupEnv: func(string) (string, bool) { return tt.distro, tt.distroOK },
				NATPrefix: func() (string, error) { return "172.19.144.0/20", tt.natErr },
				Windows:   &stubWindowsInterop{build: "26100", err: tt.buildErr},
			})
			if identity.Generation != WSL2 || tt.failed(identity).Err == nil {
				t.Fatalf("identity = %+v, want WSL2 with failed observation", identity)
			}
			if tt.name != "combined errors" {
				observations := []Observation{identity.WSLVersion, identity.Distribution, identity.WindowsBuild, identity.NetworkingMode, identity.NATPrefix}
				good := 0
				for _, observation := range observations {
					if observation.Err == nil {
						good++
					}
				}
				if good != 4 {
					t.Fatalf("identity = %+v, want exactly one failed observation", identity)
				}
			}
		})
	}
}

func TestDetectWSL1DoesNotInventANATPrefix(t *testing.T) {
	t.Parallel()

	natCalled := false
	identity := Detect(Detector{
		ReadFile: func(string) ([]byte, error) { return []byte("4.4.0-microsoft-standard"), nil },
		Command: func(_ string, args ...string) ([]byte, error) {
			if args[0] == "--version" {
				return []byte("1.2.3"), nil
			}
			return []byte("nat"), nil
		},
		LookupEnv: func(string) (string, bool) { return "Ubuntu-22.04", true },
		NATPrefix: func() (string, error) { natCalled = true; return "", nil },
		Windows:   &stubWindowsInterop{build: "26100"},
	})
	if identity.Generation != WSL1 || identity.NATPrefix.Value != notApplicable || identity.NATPrefix.Err != nil {
		t.Fatalf("identity = %+v", identity)
	}
	if natCalled {
		t.Fatal("NAT prefix reader called for WSL1")
	}
}

func TestDetectMirroredModeDoesNotLabelTheInterfacePrefixAsNAT(t *testing.T) {
	t.Parallel()

	natCalled := false
	identity := Detect(Detector{
		ReadFile: func(string) ([]byte, error) { return []byte("5.15.167.4-microsoft-standard-WSL2"), nil },
		Command: func(_ string, args ...string) ([]byte, error) {
			if args[0] == "--version" {
				return []byte("2.7.12"), nil
			}
			return []byte("mirrored"), nil
		},
		LookupEnv: func(string) (string, bool) { return "Ubuntu-24.04", true },
		NATPrefix: func() (string, error) { natCalled = true; return "", nil },
		Windows:   &stubWindowsInterop{build: "26100"},
	})
	if identity.NATPrefix.Value != notApplicable || identity.NATPrefix.Err != nil {
		t.Fatalf("NAT prefix = %+v, want not applicable", identity.NATPrefix)
	}
	if natCalled {
		t.Fatal("NAT prefix reader called for mirrored mode")
	}
}

func TestDetectUnknownNetworkingModeStillAttemptsNATPrefix(t *testing.T) {
	t.Parallel()

	natCalled := false
	identity := Detect(Detector{
		ReadFile: func(string) ([]byte, error) { return []byte("5.15.167.4-microsoft-standard-WSL2"), nil },
		Command: func(_ string, args ...string) ([]byte, error) {
			if args[0] == "--version" {
				return []byte("2.7.12"), nil
			}
			return []byte("future-mode"), nil
		},
		LookupEnv: func(string) (string, bool) { return "Ubuntu-24.04", true },
		NATPrefix: func() (string, error) {
			natCalled = true
			return "", errors.New("route unavailable")
		},
		Windows: &stubWindowsInterop{build: "26100"},
	})
	if !natCalled || identity.NATPrefix.Err == nil || identity.NATPrefix.Value == notApplicable {
		t.Fatalf("NAT prefix = %+v, called = %v; want attempted unreadable observation", identity.NATPrefix, natCalled)
	}
}
