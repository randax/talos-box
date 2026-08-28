package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/shellquote"
	"github.com/randax/talos-box/internal/version"
)

type componentIdentity struct {
	Name                 string
	Path                 string
	Version              string
	Protocol             int
	PID                  int
	Available            bool
	Detail               string
	ConfiguredPathSource string
}

type runtimeIdentity struct {
	Client, Daemon, Helper componentIdentity
	PATHTBX                []string
	Findings               []doctorFinding
}

type runtimeIdentityDeps struct {
	executable           func() (string, error)
	pathEnv              string
	command              commandOutput
	readFile             func(string) ([]byte, error)
	daemonDial           func(context.Context) (daemon.Info, int, error)
	helperDial           func(context.Context) (helper.Info, error)
	daemonProbe          func(context.Context) (daemon.Info, int, error)
	helperProbe          func(context.Context) (helper.Info, error)
	helperConfiguredPath func() configuredComponentPath
}

const runtimeProbeTimeout = 750 * time.Millisecond

type configuredComponentPath struct {
	Path   string
	Source string
}

type runtimeIdentityInactiveError struct{ detail string }

func (e runtimeIdentityInactiveError) Error() string { return e.detail }

func renderRuntimeIdentity(output io.Writer, identity runtimeIdentity) error {
	if err := renderRuntimeIdentityBlock(output, identity); err != nil {
		return err
	}
	for _, finding := range identity.Findings {
		if err := writeRuntimeIdentityFinding(output, finding); err != nil {
			return err
		}
	}
	return nil
}

func renderRuntimeIdentityBlock(output io.Writer, identity runtimeIdentity) error {
	if _, err := fmt.Fprintln(output, "runtime:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "  client: %s (%s; daemon protocol %d; helper protocol %d)\n",
		identity.Client.Path, displayVersion(identity.Client.Version), identity.Client.Protocol, helper.ProtocolVersion); err != nil {
		return err
	}
	if err := renderComponentIdentity(output, identity.Daemon); err != nil {
		return err
	}
	return renderComponentIdentity(output, identity.Helper)
}

func writeRuntimeIdentityFinding(output io.Writer, finding doctorFinding) error {
	if finding.detail == "" {
		_, err := fmt.Fprintf(output, "%s %s\n", finding.level, finding.check)
		return err
	}
	_, err := fmt.Fprintf(output, "%s %s: %s\n", finding.level, finding.check, finding.detail)
	return err
}

func renderComponentIdentity(output io.Writer, component componentIdentity) error {
	if !component.Available {
		detail := component.Detail
		if detail == "" {
			detail = "unavailable"
		}
		_, err := fmt.Fprintf(output, "  %s: %s\n", component.Name, detail)
		return err
	}

	path := component.Path
	if path == "" {
		path = "unknown path"
	}
	parts := make([]string, 0, 4)
	if component.ConfiguredPathSource != "" {
		parts = append(parts, "configured path from "+component.ConfiguredPathSource)
	}
	if component.Version != "" {
		parts = append(parts, component.Version)
	}
	parts = append(parts, formatComponentProtocol(component.Protocol))
	if component.Detail != "" {
		parts = append(parts, component.Detail)
	}
	if component.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", component.PID))
	}
	_, err := fmt.Fprintf(output, "  %s: %s (%s)\n", component.Name, path, strings.Join(parts, "; "))
	return err
}

func formatComponentProtocol(protocol int) string {
	if protocol <= 0 {
		return "protocol unknown"
	}
	return fmt.Sprintf("protocol %d", protocol)
}

func displayVersion(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func executablesOnPATH(name, pathEnv string) []string {
	seen := make(map[string]struct{})
	var paths []string
	for _, directory := range filepath.SplitList(pathEnv) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		canonical := absolute
		if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
			canonical = resolved
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		paths = append(paths, absolute)
	}
	return paths
}

func collectRuntimeIdentity(ctx context.Context, deps runtimeIdentityDeps) runtimeIdentity {
	runtimeIdentityPlatformDeps(&deps)
	identity := runtimeIdentity{
		Client: componentIdentity{Name: "client", Path: "unknown", Version: version.Version, Protocol: daemon.ProtocolVersion, Available: true},
		Daemon: componentIdentity{Name: "daemon", Detail: "not running"},
		Helper: componentIdentity{Name: "helper", Detail: "not installed"},
	}
	if deps.executable != nil {
		if path, err := deps.executable(); err == nil {
			identity.Client.Path = path
		}
	}
	identity.PATHTBX = executablesOnPATH("tbx", deps.pathEnv)
	if deps.daemonProbe != nil {
		probeCtx, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
		info, peerPID, err := deps.daemonProbe(probeCtx)
		cancel()
		if err == nil {
			identity.Daemon = componentIdentity{
				Name:      "daemon",
				Path:      info.Executable,
				Version:   info.Version,
				Protocol:  info.ProtocolVersion,
				PID:       firstPositive(info.PID, peerPID),
				Available: true,
			}
		} else {
			var inactive runtimeIdentityInactiveError
			var busy busyDaemonError
			switch {
			case errors.As(err, &inactive):
				identity.Daemon.Detail = inactive.detail
			case errors.As(err, &busy):
				identity.Daemon.Detail = fmt.Sprintf("busy (pid %d; tbx protocol %d)", busy.pid, daemon.ProtocolVersion)
			}
		}
	}
	if deps.helperProbe != nil {
		probeCtx, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
		info, err := deps.helperProbe(probeCtx)
		cancel()
		if err == nil {
			identity.Helper = componentIdentity{
				Name:      "helper",
				Path:      info.Executable,
				Version:   info.Version,
				Protocol:  info.ProtocolVersion,
				PID:       info.PID,
				Available: true,
			}
			if info.Executable == "" && info.Version == "" {
				// a wire-compatible helper built before identity reporting
				// (#492): name it through the configured install path so
				// the operator can still tell which binary is running
				identity.Helper.Detail = "predates identity reporting"
				if deps.helperConfiguredPath != nil {
					if configured := deps.helperConfiguredPath(); configured.Path != "" {
						identity.Helper.Path = configured.Path
						identity.Helper.ConfiguredPathSource = configured.Source
					}
				}
			}
		} else {
			var inactive runtimeIdentityInactiveError
			var mismatch *helper.ProtocolMismatchError
			switch {
			case errors.As(err, &inactive):
				identity.Helper = componentIdentity{Name: "helper", Detail: inactive.detail}
			case errors.As(err, &mismatch):
				path := "unknown path"
				source := ""
				if deps.helperConfiguredPath != nil {
					if configured := deps.helperConfiguredPath(); configured.Path != "" {
						path = configured.Path
						source = configured.Source
					}
				}
				identity.Helper = componentIdentity{
					Name:                 "helper",
					Path:                 path,
					Protocol:             mismatch.HelperVersion,
					Available:            true,
					Detail:               "predates identity reporting",
					ConfiguredPathSource: source,
				}
			case helperProbeUnavailable(err):
				identity.Helper = componentIdentity{Name: "helper", Detail: "not installed"}
			default:
				identity.Helper = componentIdentity{Name: "helper", Detail: "unreachable: " + err.Error()}
			}
		}
	}
	identity.Findings = runtimeIdentityFindings(identity)
	return identity
}

func helperProbeUnavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func runtimeIdentityFindings(identity runtimeIdentity) []doctorFinding {
	var findings []doctorFinding
	if len(identity.PATHTBX) > 1 {
		detail := fmt.Sprintf("multiple tbx executables on PATH: %s; choose one installation", strings.Join(identity.PATHTBX, ", "))
		if identity.Daemon.Available && identity.Daemon.Protocol == daemon.ProtocolVersion &&
			identity.Helper.Available && identity.Helper.Protocol == helper.ProtocolVersion {
			detail += "; aligning to this client makes the other installation incompatible"
		}
		findings = append(findings, doctorFinding{
			level:  "WARN",
			check:  "installations",
			detail: detail,
		})
	}
	if identity.Daemon.Available && identity.Daemon.Path != "" {
		expected := filepath.Join(filepath.Dir(identity.Client.Path), "tbxd")
		if !sameExecutable(identity.Daemon.Path, expected) {
			findings = append(findings, doctorFinding{
				level:  "WARN",
				check:  "installations",
				detail: fmt.Sprintf("daemon %s is not the sibling of client %s", identity.Daemon.Path, identity.Client.Path),
			})
		}
	}
	findings = append(findings, runtimeCompatibilityFindings(identity)...)
	return findings
}

func runtimeCompatibilityFindings(identity runtimeIdentity) []doctorFinding {
	var findings []doctorFinding
	if identity.Daemon.Available && identity.Daemon.Protocol != daemon.ProtocolVersion {
		findings = append(findings, doctorFinding{
			level:  "FAIL",
			check:  "runtime-compat",
			detail: runtimeCompatibilityDetail(identity, "daemon", identity.Daemon, daemon.ProtocolVersion),
		})
	}
	if identity.Helper.Available && identity.Helper.Protocol != helper.ProtocolVersion {
		findings = append(findings, doctorFinding{
			level:  "FAIL",
			check:  "runtime-compat",
			detail: runtimeCompatibilityDetail(identity, "helper", identity.Helper, helper.ProtocolVersion),
		})
	}
	return findings
}

func multipleInstallCompatibilityNote(identity runtimeIdentity, component string) string {
	otherInstallations := otherPATHInstallations(identity)
	if len(otherInstallations) == 0 {
		return ""
	}
	return fmt.Sprintf("aligning the %s to this client makes the other installation(s) incompatible: %s", component, strings.Join(otherInstallations, ", "))
}

func protocolDirection(found, current int) string {
	if found < current {
		return "newer"
	}
	return "older"
}

func runtimeCompatibilityDetail(identity runtimeIdentity, name string, component componentIdentity, currentProtocol int) string {
	detail := fmt.Sprintf("%s is %s than %s",
		clientCompatibilityIdentity(identity.Client),
		protocolDirection(component.Protocol, currentProtocol),
		componentCompatibilityIdentity(name, component),
	)
	if note := multipleInstallCompatibilityNote(identity, name); note != "" {
		detail += "; " + note
	}
	detail += "; run `" + compatibilityRemediation(name, identity.Client.Path) + "`"
	if name == "daemon" {
		detail += " (add `--force` only if it refuses because clusters are running)"
	}
	return detail + " or use the matching client"
}

func clientCompatibilityIdentity(component componentIdentity) string {
	return fmt.Sprintf("client %s (%s, proto %d)", component.Path, displayVersion(component.Version), component.Protocol)
}

func componentCompatibilityIdentity(name string, component componentIdentity) string {
	if component.Path == "" {
		if component.PID > 0 {
			return fmt.Sprintf("%s (pid %d, protocol %d)", name, component.PID, component.Protocol)
		}
		return fmt.Sprintf("%s (protocol %d)", name, component.Protocol)
	}
	if component.Version == "" {
		return fmt.Sprintf("%s %s (proto %d)", name, component.Path, component.Protocol)
	}
	return fmt.Sprintf("%s %s (%s, proto %d)", name, component.Path, displayVersion(component.Version), component.Protocol)
}

func compatibilityRemediation(name, clientPath string) string {
	if name == "daemon" {
		return shellquote.Quote(clientPath) + " system restart"
	}
	if runtime.GOOS == "linux" {
		return "reinstall the tbx-helper package and restart tbx-helper.socket"
	}
	return "sudo " + shellquote.Quote(clientPath) + " system install"
}

func sameExecutable(left, right string) bool {
	leftCanonical, leftErr := filepath.EvalSymlinks(left)
	rightCanonical, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil && rightErr == nil {
		return leftCanonical == rightCanonical
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

// runtimeIdentityCommand runs the host commands the identity probes need
// (systemctl on Linux). It is a variable so the package's TestMain can pin it:
// a test that talks to a fake daemon socket must not have its dial skipped
// because the CI runner's systemd has no tbxd unit.
var runtimeIdentityCommand commandOutput = execCombinedOutput

func defaultRuntimeIdentityDeps() runtimeIdentityDeps {
	return runtimeIdentityDeps{
		executable: os.Executable,
		pathEnv:    os.Getenv("PATH"),
		command:    runtimeIdentityCommand,
		readFile:   os.ReadFile,
		daemonDial: func(ctx context.Context) (daemon.Info, int, error) {
			if err := ctx.Err(); err != nil {
				return daemon.Info{}, 0, err
			}
			socketPath, err := daemon.SocketPath()
			if err != nil {
				return daemon.Info{}, 0, err
			}
			return daemonHandshakeWithin(socketPath, runtimeProbeTimeout)
		},
		helperDial: func(ctx context.Context) (helper.Info, error) {
			if err := ctx.Err(); err != nil {
				return helper.Info{}, err
			}
			return helper.Probe()
		},
	}
}

func (c cli) collectRuntimeIdentity(ctx context.Context) runtimeIdentity {
	if c.runtimeIdentity != nil {
		return c.runtimeIdentity(ctx)
	}
	deps := defaultRuntimeIdentityDeps()
	if c.runtimeIdentityDeps != nil {
		deps = *c.runtimeIdentityDeps
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*runtimeProbeTimeout)
	defer cancel()
	return collectRuntimeIdentity(probeCtx, deps)
}

func otherPATHInstallations(identity runtimeIdentity) []string {
	if len(identity.PATHTBX) <= 1 {
		return nil
	}
	others := make([]string, 0, len(identity.PATHTBX)-1)
	for _, path := range identity.PATHTBX {
		if identity.Client.Path != "" && sameExecutable(path, identity.Client.Path) {
			continue
		}
		others = append(others, path)
	}
	return others
}
