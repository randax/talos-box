package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		"  client: /opt/current/tbx (0.1.3; tbx protocol %d; daemon protocol %d; helper protocol %d)\n"+
		"  daemon: /opt/current/tbxd (0.1.3; protocol %d; pid 1234)\n"+
		"  helper: /opt/current/tbx-helper (0.1.3; protocol %d; pid 2345)\n",
		daemon.ProtocolVersion, daemon.ProtocolVersion, helper.ProtocolVersion, daemon.ProtocolVersion, helper.ProtocolVersion)
	if output.String() != want {
		t.Fatalf("renderRuntimeIdentity() =\n%s\nwant:\n%s", output.String(), want)
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

func TestRuntimeIdentityReportsUnavailableComponentsWithoutStartingThem(t *testing.T) {
	var daemonProbes, helperProbes int
	identity := collectRuntimeIdentity(context.Background(), runtimeIdentityDeps{
		executable: func() (string, error) { return "/opt/current/tbx", nil },
		daemonProbe: func(context.Context) (daemon.Info, int, error) {
			daemonProbes++
			return daemon.Info{}, 0, fmt.Errorf("connection refused")
		},
		helperProbe: func(context.Context) (helper.Info, error) {
			helperProbes++
			return helper.Info{}, fmt.Errorf("no helper socket")
		},
	})
	var output bytes.Buffer
	if err := renderRuntimeIdentity(&output, identity); err != nil {
		t.Fatal(err)
	}
	if daemonProbes != 1 || helperProbes != 1 {
		t.Fatalf("probe calls = daemon %d helper %d, want one read-only probe each", daemonProbes, helperProbes)
	}
	for _, want := range []string{"daemon: not running", "helper: not installed"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("runtime output missing %q:\n%s", want, output.String())
		}
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

func TestRuntimeIdentityNamesBothSidesOfOlderDaemonAndHelperMismatch(t *testing.T) {
	identity := collectRuntimeIdentity(context.Background(), runtimeIdentityDeps{
		executable: func() (string, error) { return "/opt/current/tbx", nil },
		daemonProbe: func(context.Context) (daemon.Info, int, error) {
			return daemon.Info{
				ProtocolVersion: daemon.ProtocolVersion - 1,
				Version:         "0.1.2",
				Executable:      "/opt/old/tbxd",
				PID:             1111,
			}, 1111, nil
		},
		helperProbe: func(context.Context) (helper.Info, error) {
			return helper.Info{
				ProtocolVersion: helper.ProtocolVersion - 1,
				Version:         "0.1.2",
				Executable:      "/opt/old/tbx-helper",
				PID:             2222,
			}, nil
		},
	})
	var output bytes.Buffer
	if err := renderRuntimeIdentity(&output, identity); err != nil {
		t.Fatal(err)
	}
	want := "FAIL runtime-compat: client /opt/current/tbx is newer than daemon /opt/old/tbxd (protocol 16 < 17) and helper /opt/old/tbx-helper (protocol 4 < 5); run `/opt/current/tbx system restart --force` or use the matching client\n"
	if !strings.Contains(output.String(), want) {
		t.Fatalf("runtime output =\n%s\nwant line:\n%s", output.String(), want)
	}
}

func TestRuntimeIdentityDoesNotPrescribeHelperReinstallBeforeNamingMultipleInstallConflict(t *testing.T) {
	identity := runtimeIdentity{
		Client:  componentIdentity{Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
		Helper:  componentIdentity{Path: "/opt/old/tbx-helper", Version: "0.1.2", Protocol: helper.ProtocolVersion - 1, Available: true},
		PATHTBX: []string{"/opt/current/tbx", "/opt/old/tbx"},
	}
	findings := runtimeIdentityFindings(identity)
	if len(findings) != 2 || findings[0].check != "installations" || findings[1].check != "runtime-compat" {
		t.Fatalf("findings = %+v, want installation conflict before compatibility failure", findings)
	}
	if strings.Contains(findings[0].detail, "system install") || strings.Contains(findings[0].detail, "reinstall the helper") {
		t.Fatalf("installations detail = %q, want the multi-install conflict named first", findings[0].detail)
	}
}
