package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/shellquote"
	"github.com/randax/talos-box/internal/version"
)

type componentIdentity struct {
	Name      string
	Path      string
	Version   string
	Protocol  int
	PID       int
	Available bool
	Detail    string
}

type runtimeIdentity struct {
	Client, Daemon, Helper componentIdentity
	PATHTBX                []string
	Findings               []doctorFinding
}

type runtimeIdentityDeps struct {
	executable  func() (string, error)
	pathEnv     string
	daemonProbe func(context.Context) (daemon.Info, int, error)
	helperProbe func(context.Context) (helper.Info, error)
}

const runtimeProbeTimeout = 750 * time.Millisecond

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
	if _, err := fmt.Fprintf(output, "  client: %s (%s; tbx protocol %d; daemon protocol %d; helper protocol %d)\n",
		identity.Client.Path, displayVersion(identity.Client.Version), identity.Client.Protocol, identity.Client.Protocol, helper.ProtocolVersion); err != nil {
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
		path = "unknown"
	}
	parts := make([]string, 0, 3)
	if component.Path == "" || component.Version == "" {
		parts = append(parts, component.Name+" predates identity reporting")
	} else {
		parts = append(parts, component.Version)
	}
	parts = append(parts, formatComponentProtocol(component.Protocol))
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
			var busy busyDaemonError
			if errors.As(err, &busy) {
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
		}
	}
	identity.Findings = runtimeIdentityFindings(identity)
	return identity
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
		findings = append(findings, doctorFinding{
			level:  "WARN",
			check:  "installations",
			detail: fmt.Sprintf("multiple tbx executables on PATH: %s; choose one installation", strings.Join(identity.PATHTBX, ", ")),
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
	if detail := runtimeCompatibilityDetail(identity); detail != "" {
		findings = append(findings, doctorFinding{
			level:  "FAIL",
			check:  "runtime-compat",
			detail: detail,
		})
	}
	return findings
}

func runtimeCompatibilityDetail(identity runtimeIdentity) string {
	var components []string
	direction := ""
	if identity.Daemon.Available && identity.Daemon.Protocol != daemon.ProtocolVersion {
		components = append(components, formatProtocolSkew("daemon", identity.Daemon.Path, identity.Daemon.Protocol, daemon.ProtocolVersion))
		direction = protocolDirection(identity.Daemon.Protocol, daemon.ProtocolVersion)
	}
	if identity.Helper.Available && identity.Helper.Protocol != helper.ProtocolVersion {
		components = append(components, formatProtocolSkew("helper", identity.Helper.Path, identity.Helper.Protocol, helper.ProtocolVersion))
		if direction == "" {
			direction = protocolDirection(identity.Helper.Protocol, helper.ProtocolVersion)
		}
	}
	if len(components) == 0 {
		return ""
	}
	command := shellquote.Quote(identity.Client.Path) + " system restart --force"
	return fmt.Sprintf("client %s is %s than %s; run `%s` or use the matching client",
		identity.Client.Path, direction, joinWithAnd(components), command)
}

func protocolDirection(found, current int) string {
	if found < current {
		return "newer"
	}
	return "older"
}

func formatProtocolSkew(name, path string, found, current int) string {
	if path == "" {
		path = "unknown"
	}
	return fmt.Sprintf("%s %s (protocol %d %s %d)", name, path, found, compareSymbol(found, current), current)
}

func compareSymbol(found, current int) string {
	if found < current {
		return "<"
	}
	return ">"
}

func joinWithAnd(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func sameExecutable(left, right string) bool {
	leftCanonical, leftErr := filepath.EvalSymlinks(left)
	rightCanonical, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil && rightErr == nil {
		return leftCanonical == rightCanonical
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func defaultRuntimeIdentityDeps() runtimeIdentityDeps {
	return runtimeIdentityDeps{
		executable: os.Executable,
		pathEnv:    os.Getenv("PATH"),
		daemonProbe: func(ctx context.Context) (daemon.Info, int, error) {
			if err := ctx.Err(); err != nil {
				return daemon.Info{}, 0, err
			}
			socketPath, err := daemon.SocketPath()
			if err != nil {
				return daemon.Info{}, 0, err
			}
			return daemonHandshakeWithin(socketPath, runtimeProbeTimeout)
		},
		helperProbe: func(ctx context.Context) (helper.Info, error) {
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
