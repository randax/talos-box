package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/helper"
)

func TestRuntimeIdentityPrintsClientDaemonAndHelperTogether(t *testing.T) {
	identity := runtimeIdentity{
		Client: componentIdentity{Name: "client", Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
		Daemon: componentIdentity{Name: "daemon", Path: "/opt/current/tbxd", Version: "0.1.3", Protocol: daemon.ProtocolVersion, PID: 1234, Available: true},
		Helper: componentIdentity{Name: "helper", Path: "/opt/current/tbx-helper", Version: "0.1.3", Protocol: helper.ProtocolVersion, PID: 2345, Available: true},
	}
	var output bytes.Buffer
	if err := renderRuntimeIdentity(&output, identity); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("runtime:\n"+
		"  client: /opt/current/tbx (0.1.3; daemon protocol %d; helper protocol %d)\n"+
		"  daemon: /opt/current/tbxd (0.1.3; protocol %d; pid 1234)\n"+
		"  helper: /opt/current/tbx-helper (0.1.3; protocol %d; pid 2345)\n",
		daemon.ProtocolVersion, helper.ProtocolVersion, daemon.ProtocolVersion, helper.ProtocolVersion)
	if output.String() != want {
		t.Fatalf("renderRuntimeIdentity() =\n%s\nwant:\n%s", output.String(), want)
	}
	if strings.Contains(output.String(), "tbx protocol") {
		t.Fatalf("renderRuntimeIdentity() kept the duplicated tbx protocol line:\n%s", output.String())
	}
}

func TestRuntimeIdentityWarnsForMultipleDistinctTBXOnPATH(t *testing.T) {
	first := filepath.Join(t.TempDir(), "tbx")
	second := filepath.Join(t.TempDir(), "tbx")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("test"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := executablesOnPATH("tbx", strings.Join([]string{filepath.Dir(first), filepath.Dir(second)}, string(os.PathListSeparator)))
	if !slices.Equal(got, []string{first, second}) {
		t.Fatalf("executablesOnPATH() = %v, want PATH order %v", got, []string{first, second})
	}
}

func TestRuntimeIdentityDeduplicatesSymlinkedPATHEntries(t *testing.T) {
	root := t.TempDir()
	firstDir := filepath.Join(root, "first")
	secondDir := filepath.Join(root, "second")
	for _, directory := range []string{firstDir, secondDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(firstDir, "tbx")
	if err := os.WriteFile(target, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(secondDir, "tbx")); err != nil {
		t.Fatal(err)
	}

	got := executablesOnPATH("tbx", strings.Join([]string{secondDir, firstDir}, string(os.PathListSeparator)))
	if !slices.Equal(got, []string{filepath.Join(secondDir, "tbx")}) {
		t.Fatalf("executablesOnPATH() = %v, want one canonical entry in PATH order", got)
	}
}

func TestRuntimeIdentityReportsUnavailableAndUnreachableHelpers(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "missing socket", err: fmt.Errorf("connect to helper at /tmp/tbx-helper.sock: %w", os.ErrNotExist), want: "helper: not installed"},
		{name: "connection refused", err: fmt.Errorf("connect to helper at /tmp/tbx-helper.sock: %w", syscall.ECONNREFUSED), want: "helper: not installed"},
		{name: "other error", err: errors.New("tls alert"), want: "helper: unreachable: tls alert"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity := collectRuntimeIdentity(context.Background(), runtimeIdentityDeps{
				executable: func() (string, error) { return "/opt/current/tbx", nil },
				helperProbe: func(context.Context) (helper.Info, error) {
					return helper.Info{}, test.err
				},
			})
			var output bytes.Buffer
			if err := renderRuntimeIdentity(&output, identity); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), test.want) {
				t.Fatalf("runtime output = %q, want %q", output.String(), test.want)
			}
		})
	}
}

func TestRuntimeIdentityTreatsRejectedOldHelperAsAvailable(t *testing.T) {
	identity := collectRuntimeIdentity(context.Background(), runtimeIdentityDeps{
		executable: func() (string, error) { return "/opt/current/tbx", nil },
		helperProbe: func(context.Context) (helper.Info, error) {
			return helper.Info{}, &helper.ProtocolMismatchError{ClientVersion: helper.ProtocolVersion, HelperVersion: helper.ProtocolVersion - 1}
		},
		helperConfiguredPath: func() configuredComponentPath { return configuredComponentPath{} },
	})
	var output bytes.Buffer
	if err := renderRuntimeIdentity(&output, identity); err != nil {
		t.Fatal(err)
	}
	if !identity.Helper.Available {
		t.Fatal("helper mismatch rendered as unavailable")
	}
	if got := output.String(); !strings.Contains(got, fmt.Sprintf("helper: unknown path (protocol %d; predates identity reporting)", helper.ProtocolVersion-1)) {
		t.Fatalf("runtime output = %q, want old-helper identity line", got)
	}
	if got := output.String(); !strings.Contains(got, "FAIL runtime-compat:") {
		t.Fatalf("runtime output = %q, want runtime-compat finding", got)
	}
	// the remediation text is platform-specific (macOS reinstalls through
	// sudo, Linux through the package); assert through the same helper the
	// renderer uses so the test holds on every CI runner
	if got := output.String(); !strings.Contains(got, compatibilityRemediation("helper", "/opt/current/tbx")) || strings.Contains(got, "system restart") {
		t.Fatalf("runtime output = %q, want helper remediation without daemon restart", got)
	}
	if strings.Contains(output.String(), "helper: not installed") {
		t.Fatalf("runtime output = %q, must not hide a reachable old helper as not installed", output.String())
	}
}

func TestRuntimeIdentityMismatchedOldHelperShowsConfiguredPathAndCompetingInstallWarningBeforeRemediation(t *testing.T) {
	firstDir := filepath.Join(t.TempDir(), "current")
	secondDir := filepath.Join(t.TempDir(), "other")
	for _, directory := range []string{firstDir, secondDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(firstDir, "tbx"), filepath.Join(secondDir, "tbx")} {
		if err := os.WriteFile(path, []byte("test"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	deps := runtimeIdentityDeps{
		executable: func() (string, error) { return filepath.Join(firstDir, "tbx"), nil },
		pathEnv:    strings.Join([]string{firstDir, secondDir}, string(os.PathListSeparator)),
		helperProbe: func(context.Context) (helper.Info, error) {
			return helper.Info{}, &helper.ProtocolMismatchError{ClientVersion: helper.ProtocolVersion, HelperVersion: helper.ProtocolVersion - 1}
		},
		helperConfiguredPath: func() configuredComponentPath {
			return configuredComponentPath{Path: "/opt/old/tbx-helper", Source: "launchd"}
		},
	}

	identity := collectRuntimeIdentity(context.Background(), deps)
	var output bytes.Buffer
	if err := renderRuntimeIdentity(&output, identity); err != nil {
		t.Fatal(err)
	}
	text := output.String()

	helperLine := fmt.Sprintf("helper: /opt/old/tbx-helper (configured path from launchd; protocol %d; predates identity reporting)", helper.ProtocolVersion-1)
	warning := "aligning the helper to this client makes the other installation(s) incompatible: " + filepath.Join(secondDir, "tbx")
	remediation := compatibilityRemediation("helper", filepath.Join(firstDir, "tbx"))
	for _, want := range []string{helperLine, warning, remediation} {
		if !strings.Contains(text, want) {
			t.Fatalf("runtime output missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, helperLine) >= strings.Index(text, warning) || strings.Index(text, warning) >= strings.Index(text, remediation) {
		t.Fatalf("runtime output order wrong:\n%s", text)
	}
}

func TestRuntimeIdentityTreatsRejectedOldHelperAsAvailableWithConfiguredPath(t *testing.T) {
	identity := collectRuntimeIdentity(context.Background(), runtimeIdentityDeps{
		executable: func() (string, error) { return "/opt/current/tbx", nil },
		helperProbe: func(context.Context) (helper.Info, error) {
			return helper.Info{}, &helper.ProtocolMismatchError{ClientVersion: helper.ProtocolVersion, HelperVersion: helper.ProtocolVersion - 1}
		},
		helperConfiguredPath: func() configuredComponentPath {
			return configuredComponentPath{Path: "/usr/local/bin/tbx-helper", Source: "launchd"}
		},
	})
	var output bytes.Buffer
	if err := renderRuntimeIdentity(&output, identity); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("helper: /usr/local/bin/tbx-helper (configured path from launchd; protocol %d; predates identity reporting)", helper.ProtocolVersion-1)
	if got := output.String(); !strings.Contains(got, want) {
		t.Fatalf("runtime output = %q, want %q", got, want)
	}
}

func TestRuntimeIdentityWarnsWhenDaemonIsNotClientSibling(t *testing.T) {
	identity := runtimeIdentity{
		Client: componentIdentity{Path: "/opt/current/tbx", Protocol: daemon.ProtocolVersion, Available: true},
		Daemon: componentIdentity{Path: "/opt/other/tbxd", Protocol: daemon.ProtocolVersion, Available: true},
	}
	findings := runtimeIdentityFindings(identity)
	if len(findings) != 1 || findings[0].level != "WARN" || findings[0].check != "installations" {
		t.Fatalf("runtimeIdentityFindings() = %+v, want one installations warning", findings)
	}
	for _, want := range []string{"/opt/other/tbxd", "/opt/current/tbx", "not the sibling"} {
		if !strings.Contains(findings[0].detail, want) {
			t.Errorf("warning %q missing %q", findings[0].detail, want)
		}
	}
}

func TestRuntimeIdentityWarnsForMultipleDistinctTBXOnPATHFinding(t *testing.T) {
	identity := runtimeIdentity{PATHTBX: []string{"/opt/first/tbx", "/opt/second/tbx"}}
	findings := runtimeIdentityFindings(identity)
	if len(findings) != 1 || findings[0].level != "WARN" || findings[0].check != "installations" {
		t.Fatalf("runtimeIdentityFindings() = %+v, want one installations warning", findings)
	}
	if !strings.Contains(findings[0].detail, "/opt/first/tbx, /opt/second/tbx") {
		t.Fatalf("warning %q missing PATH order", findings[0].detail)
	}
}

func TestRuntimeIdentityCompatibilityFindingsArePerComponent(t *testing.T) {
	identity := runtimeIdentity{
		Client: componentIdentity{Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
		Daemon: componentIdentity{Path: "/opt/old/tbxd", Version: "0.1.2", Protocol: daemon.ProtocolVersion - 1, Available: true},
		Helper: componentIdentity{Path: "/opt/old/tbx-helper", Version: "0.1.2", Protocol: helper.ProtocolVersion - 1, Available: true},
	}
	findings := runtimeIdentityFindings(identity)
	if len(findings) != 3 {
		t.Fatalf("findings = %+v, want 3", findings)
	}
	if findings[1].check != "runtime-compat" || !strings.Contains(findings[1].detail, "daemon /opt/old/tbxd (0.1.2, proto") {
		t.Fatalf("daemon finding = %+v", findings[1])
	}
	if findings[2].check != "runtime-compat" || !strings.Contains(findings[2].detail, "helper /opt/old/tbx-helper (0.1.2, proto") {
		t.Fatalf("helper finding = %+v", findings[2])
	}
}

func TestRuntimeIdentityFormatsDaemonOnlyMismatchBothDirections(t *testing.T) {
	tests := []struct {
		name      string
		protocol  int
		direction string
	}{
		{name: "client newer", protocol: daemon.ProtocolVersion - 1, direction: "newer"},
		{name: "client older", protocol: daemon.ProtocolVersion + 1, direction: "older"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity := runtimeIdentity{
				Client: componentIdentity{Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
				Daemon: componentIdentity{Path: "/opt/current/tbxd", Version: "0.1.2", Protocol: test.protocol, Available: true},
				Helper: componentIdentity{Protocol: helper.ProtocolVersion, Available: true},
			}
			findings := runtimeIdentityFindings(identity)
			if len(findings) != 1 || findings[0].check != "runtime-compat" {
				t.Fatalf("findings = %+v, want one daemon compatibility failure", findings)
			}
			for _, want := range []string{
				"client /opt/current/tbx (0.1.3, proto",
				"is " + test.direction + " than daemon /opt/current/tbxd (0.1.2, proto",
				"`/opt/current/tbx system restart`",
				"add `--force` only if it refuses because clusters are running",
			} {
				if !strings.Contains(findings[0].detail, want) {
					t.Fatalf("daemon detail = %q, want %q", findings[0].detail, want)
				}
			}
		})
	}
}

func TestRuntimeIdentityCompatibilityFindingUsesDaemonPIDWhenPathUnknown(t *testing.T) {
	identity := runtimeIdentity{
		Client: componentIdentity{Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
		Daemon: componentIdentity{PID: 4242, Protocol: daemon.ProtocolVersion - 1, Available: true},
	}
	findings := runtimeIdentityFindings(identity)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one daemon compatibility finding", findings)
	}
	if got := findings[0].detail; !strings.Contains(got, "daemon (pid 4242, protocol") || strings.Contains(got, "daemon unknown") {
		t.Fatalf("daemon finding = %q, want pid wording without unknown", got)
	}
}

func TestRuntimeIdentityNotesOtherPATHInstallWillBecomeIncompatible(t *testing.T) {
	aligned := runtimeIdentity{
		Client:  componentIdentity{Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
		Daemon:  componentIdentity{Path: "/opt/current/tbxd", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
		Helper:  componentIdentity{Path: "/opt/current/tbx-helper", Version: "0.1.3", Protocol: helper.ProtocolVersion, Available: true},
		PATHTBX: []string{"/opt/current/tbx", "/opt/old/tbx"},
	}
	alignedFindings := runtimeIdentityFindings(aligned)
	if len(alignedFindings) != 1 || !strings.Contains(alignedFindings[0].detail, "aligning to this client makes the other installation incompatible") {
		t.Fatalf("aligned findings = %+v, want existing competing-install note", alignedFindings)
	}

	identity := runtimeIdentity{
		Client:  componentIdentity{Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
		Helper:  componentIdentity{Path: "/opt/current/tbx-helper", Version: "0.1.2", Protocol: helper.ProtocolVersion - 1, Available: true},
		PATHTBX: []string{"/opt/current/tbx", "/opt/old/tbx"},
	}
	findings := runtimeIdentityFindings(identity)
	if len(findings) != 2 || findings[1].check != "runtime-compat" {
		t.Fatalf("findings = %+v, want installation warning plus helper mismatch", findings)
	}
	if !strings.Contains(findings[1].detail, "aligning the helper to this client makes the other installation(s) incompatible: /opt/old/tbx") {
		t.Fatalf("runtime-compat detail = %q, want competing-install note", findings[1].detail)
	}
}

func TestRuntimeIdentityNotesOtherPATHInstallWillBecomeIncompatibleForMismatches(t *testing.T) {
	tests := []struct {
		name      string
		component string
		identity  runtimeIdentity
	}{
		{
			name:      "daemon mismatch",
			component: "daemon",
			identity: runtimeIdentity{
				Client:  componentIdentity{Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
				Daemon:  componentIdentity{Path: "/opt/old/tbxd", Version: "0.1.2", Protocol: daemon.ProtocolVersion - 1, Available: true},
				Helper:  componentIdentity{Path: "/opt/current/tbx-helper", Version: "0.1.3", Protocol: helper.ProtocolVersion, Available: true},
				PATHTBX: []string{"/opt/current/tbx", "/opt/old/tbx"},
			},
		},
		{
			name:      "helper mismatch",
			component: "helper",
			identity: runtimeIdentity{
				Client:  componentIdentity{Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
				Daemon:  componentIdentity{Path: "/opt/current/tbxd", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
				Helper:  componentIdentity{Path: "/opt/old/tbx-helper", Version: "0.1.2", Protocol: helper.ProtocolVersion - 1, Available: true},
				PATHTBX: []string{"/opt/current/tbx", "/opt/old/tbx"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			findings := runtimeIdentityFindings(test.identity)
			if len(findings) < 2 {
				t.Fatalf("findings = %+v, want installations and runtime-compat", findings)
			}
			runtimeFinding := findings[len(findings)-1]
			want := "aligning the " + test.component + " to this client makes the other installation(s) incompatible: /opt/old/tbx"
			if !strings.Contains(runtimeFinding.detail, want) {
				t.Fatalf("runtime finding = %q, want %q", runtimeFinding.detail, want)
			}
		})
	}
}

func TestRuntimeIdentityNamesCompatibleHelperWithoutIdentityThroughConfiguredPath(t *testing.T) {
	identity := collectRuntimeIdentity(context.Background(), runtimeIdentityDeps{
		executable: func() (string, error) { return "/opt/current/tbx", nil },
		helperProbe: func(context.Context) (helper.Info, error) {
			return helper.Info{ProtocolVersion: helper.ProtocolVersion}, nil
		},
		helperConfiguredPath: func() configuredComponentPath {
			return configuredComponentPath{Path: "/opt/current/tbx-helper", Source: "launchd"}
		},
	})
	var output bytes.Buffer
	if err := renderRuntimeIdentity(&output, identity); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("helper: /opt/current/tbx-helper (configured path from launchd; protocol %d; predates identity reporting)", helper.ProtocolVersion)
	if got := output.String(); !strings.Contains(got, want) {
		t.Fatalf("runtime output = %q, want %q", got, want)
	}
	if strings.Contains(output.String(), "FAIL runtime-compat") {
		t.Fatalf("runtime output = %q, a wire-compatible helper must not fail runtime-compat", output.String())
	}
}
