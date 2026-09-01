// Package wsl reports the WSL identity visible from the Linux substrate.
package wsl

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Generation identifies whether the Linux kernel belongs to WSL and, if so,
// which WSL generation it reports.
type Generation uint8

const (
	NotWSL Generation = iota
	WSL1
	WSL2
)

const notApplicable = "not applicable"

// knownNonNATNetworkingModes holds every mode string wslinfo can emit besides
// nat (per Microsoft's wslinfo.cpp: bridged, mirrored, consomme, none, wsl1)
// plus virtioproxy, Consomme's .wslconfig spelling, in case an older build
// echoes the config value. An unknown future mode is not proof NAT is
// inapplicable, so it still attempts the Linux-side prefix derivation.
var knownNonNATNetworkingModes = map[string]struct{}{
	"bridged":     {},
	"consomme":    {},
	"mirrored":    {},
	"none":        {},
	"virtioproxy": {},
	"wsl1":        {},
}

// Observation keeps an independently collected identity field and its
// collection error. Callers render stable text instead of host-specific errors.
type Observation struct {
	Value string
	Err   error
}

// Identity is the Linux-side snapshot used by WSL-aware checks (#553).
type Identity struct {
	Generation     Generation
	KernelRelease  string
	WSLVersion     Observation
	Distribution   Observation
	WindowsBuild   Observation
	NetworkingMode Observation
	NATPrefix      Observation
}

func (i Identity) IsWSL() bool  { return i.Generation != NotWSL }
func (i Identity) IsWSL2() bool { return i.Generation == WSL2 }

// GenerationFromOSRelease recognizes Microsoft's kernel marker
// case-insensitively; WSL2 adds its own WSL2 marker.
func GenerationFromOSRelease(osrelease string) Generation {
	release := strings.ToLower(strings.TrimSpace(osrelease))
	if !strings.Contains(release, "microsoft") {
		return NotWSL
	}
	if strings.Contains(release, "wsl2") {
		return WSL2
	}
	return WSL1
}

type ReadFile func(string) ([]byte, error)
type Command func(name string, args ...string) ([]byte, error)

// WindowsInterop is the only WSL-to-Windows crossing. Future Windows
// integration must extend this semantic boundary instead of invoking Windows
// tools elsewhere (#553).
type WindowsInterop interface {
	WindowsBuild() (string, error)
}

// Detector contains every host-facing dependency used by Detect.
type Detector struct {
	ReadFile  ReadFile
	Command   Command
	LookupEnv func(string) (string, bool)
	NATPrefix func() (string, error)
	Windows   WindowsInterop
}

// Detect collects all independent WSL observations. wslinfo is Linux-side;
// WindowsInterop is the sole Windows crossing.
func Detect(deps Detector) Identity {
	identity := Identity{}
	if deps.ReadFile == nil {
		return identity
	}
	release, err := deps.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return identity
	}
	identity.KernelRelease = strings.TrimSpace(string(release))
	identity.Generation = GenerationFromOSRelease(identity.KernelRelease)
	if !identity.IsWSL() {
		return identity
	}

	identity.WSLVersion = commandObservation(deps.Command, "wslinfo", "--version")
	if deps.LookupEnv == nil {
		identity.Distribution.Err = errors.New("environment lookup unavailable")
	} else if distro, ok := deps.LookupEnv("WSL_DISTRO_NAME"); !ok || strings.TrimSpace(distro) == "" {
		identity.Distribution.Err = errors.New("WSL_DISTRO_NAME is unset")
	} else {
		identity.Distribution.Value = strings.TrimSpace(distro)
	}
	if deps.Windows == nil {
		identity.WindowsBuild.Err = errors.New("interop with Windows is unavailable")
	} else {
		identity.WindowsBuild.Value, identity.WindowsBuild.Err = deps.Windows.WindowsBuild()
		identity.WindowsBuild.Value = strings.TrimSpace(identity.WindowsBuild.Value)
		if identity.WindowsBuild.Err == nil && identity.WindowsBuild.Value == "" {
			identity.WindowsBuild.Err = errors.New("the Windows build value is empty")
		}
	}
	identity.NetworkingMode = commandObservation(deps.Command, "wslinfo", "--networking-mode")
	identity.NetworkingMode.Value = strings.ToLower(identity.NetworkingMode.Value)

	switch {
	case identity.Generation == WSL1:
		identity.NATPrefix.Value = notApplicable
	case identity.NetworkingMode.Err == nil && knownNonNATNetworkingMode(identity.NetworkingMode.Value):
		identity.NATPrefix.Value = notApplicable
	case deps.NATPrefix == nil:
		identity.NATPrefix.Err = errors.New("NAT prefix reader unavailable")
	default:
		identity.NATPrefix.Value, identity.NATPrefix.Err = deps.NATPrefix()
		identity.NATPrefix.Value = strings.TrimSpace(identity.NATPrefix.Value)
		if identity.NATPrefix.Err == nil && identity.NATPrefix.Value == "" {
			identity.NATPrefix.Err = errors.New("NAT prefix is empty")
		}
	}
	return identity
}

func knownNonNATNetworkingMode(mode string) bool {
	_, ok := knownNonNATNetworkingModes[mode]
	return ok
}

func commandObservation(command Command, name string, args ...string) Observation {
	if command == nil {
		return Observation{Err: errors.New("command runner unavailable")}
	}
	output, err := command(name, args...)
	value := strings.TrimSpace(string(output))
	if err != nil {
		return Observation{Err: err}
	}
	if value == "" {
		return Observation{Err: fmt.Errorf("%s output is empty", name)}
	}
	return Observation{Value: value}
}

// SystemDetector wires portable detection to the supplied bounded doctor
// subprocess and file seams. wslinfo remains a Linux-side observation.
func SystemDetector(readFile ReadFile, command Command) Detector {
	return Detector{
		ReadFile:  readFile,
		Command:   command,
		LookupEnv: os.LookupEnv,
		NATPrefix: func() (string, error) { return natPrefixFrom(readFile, systemInterfaceAddrs) },
		Windows:   NewWindowsInterop(command),
	}
}
