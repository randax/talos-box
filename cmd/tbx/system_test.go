package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/resolverset"
)

func uidPtr(uid uint32) *uint32 { return &uid }

type fakeDNSUninstaller struct {
	calls    int
	closed   bool
	uninstal error
}

func TestSystemStatusUsesTheRuntimeIdentityBlock(t *testing.T) {
	runtimeDeps := runtimeIdentityDeps{
		executable: func() (string, error) { return "/opt/current/tbx", nil },
		daemonProbe: func(context.Context) (daemon.Info, int, error) {
			return daemon.Info{
				ProtocolVersion: daemon.ProtocolVersion,
				Version:         "0.1.3",
				Executable:      "/opt/current/tbxd",
				PID:             1234,
			}, 1234, nil
		},
		helperProbe: func(context.Context) (helper.Info, error) {
			return helper.Info{
				ProtocolVersion: helper.ProtocolVersion,
				Version:         "0.1.3",
				Executable:      "/opt/current/tbx-helper",
				PID:             2345,
			}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr, runtimeIdentityDeps: &runtimeDeps}
	if err := command.run([]string{"system", "status"}); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	for _, want := range []string{
		"runtime:\n",
		"client: /opt/current/tbx",
		"daemon: /opt/current/tbxd (0.1.3; protocol 17; pid 1234)",
		"helper: /opt/current/tbx-helper (0.1.3; protocol 5; pid 2345)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("system status output missing %q:\n%s", want, text)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestSystemStatusPrintsRestartHintOnlyForDaemonMismatch(t *testing.T) {
	tests := []struct {
		name     string
		identity runtimeIdentity
		wantHint bool
	}{
		{
			name: "helper only",
			identity: runtimeIdentity{
				Client: componentIdentity{Name: "client", Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
				Daemon: componentIdentity{Name: "daemon", Path: "/opt/current/tbxd", Version: "0.1.3", Protocol: daemon.ProtocolVersion, PID: 1234, Available: true},
				Helper: componentIdentity{Name: "helper", Path: "/opt/old/tbx-helper", Version: "0.1.2", Protocol: helper.ProtocolVersion - 1, Available: true},
				Findings: []doctorFinding{
					{level: "FAIL", check: "runtime-compat", detail: "client /opt/current/tbx (0.1.3, proto 17) is newer than helper /opt/old/tbx-helper (0.1.2, proto 4); run `sudo /opt/current/tbx system install` or use the matching client"},
				},
			},
			wantHint: false,
		},
		{
			name: "daemon only",
			identity: runtimeIdentity{
				Client: componentIdentity{Name: "client", Path: "/opt/current/tbx", Version: "0.1.3", Protocol: daemon.ProtocolVersion, Available: true},
				Daemon: componentIdentity{Name: "daemon", Path: "/opt/old/tbxd", Version: "0.1.2", Protocol: daemon.ProtocolVersion - 1, PID: 1234, Available: true},
				Helper: componentIdentity{Name: "helper", Path: "/opt/current/tbx-helper", Version: "0.1.3", Protocol: helper.ProtocolVersion, PID: 2345, Available: true},
				Findings: []doctorFinding{
					{level: "FAIL", check: "runtime-compat", detail: "client /opt/current/tbx (0.1.3, proto 17) is newer than daemon /opt/old/tbxd (0.1.2, proto 16); run `/opt/current/tbx system restart` (add `--force` only if it refuses because clusters are running) or use the matching client"},
				},
			},
			wantHint: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			command := cli{
				out:             &stdout,
				err:             &stderr,
				runtimeIdentity: func(context.Context) runtimeIdentity { return test.identity },
			}

			if err := command.run([]string{"system", "status"}); err != nil {
				t.Fatal(err)
			}
			gotHint := strings.Contains(stderr.String(), "warning: run: tbx system restart")
			if gotHint != test.wantHint {
				t.Fatalf("stderr = %q, want hint=%v", stderr.String(), test.wantHint)
			}
		})
	}
}

func (f *fakeDNSUninstaller) UninstallDNS() error {
	f.calls++
	return f.uninstal
}

func (f *fakeDNSUninstaller) Close() error {
	f.closed = true
	return nil
}

func TestRemoveScopedResolverUninstallsThroughHelper(t *testing.T) {
	t.Parallel()

	client := &fakeDNSUninstaller{}
	var stderr bytes.Buffer
	command := cli{out: &bytes.Buffer{}, err: &stderr}
	for range 2 {
		if err := command.removeScopedResolver(func() (dnsUninstaller, error) {
			return client, nil
		}, func() error {
			t.Fatal("fallback must not run when the helper is reachable")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if client.calls != 2 {
		t.Fatalf("UninstallDNS calls = %d, want 2", client.calls)
	}
	if !client.closed {
		t.Fatal("helper connection was not closed")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRemoveScopedResolverFallsBackWhenHelperIsUnreachable(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	fallbackCalls := 0
	command := cli{out: &bytes.Buffer{}, err: &stderr}
	if err := command.removeScopedResolver(func() (dnsUninstaller, error) {
		return nil, errors.New("connect to helper at /var/run/tbx-helper.sock: no such file")
	}, func() error {
		fallbackCalls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fallbackCalls != 1 {
		t.Fatalf("fallback calls = %d, want 1", fallbackCalls)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRemoveScopedResolverWarnsWhenFallbackFails(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	command := cli{out: &bytes.Buffer{}, err: &stderr}
	err := command.removeScopedResolver(func() (dnsUninstaller, error) {
		return nil, errors.New("connect to helper at /var/run/tbx-helper.sock: no such file")
	}, func() error {
		return errors.New("remove /etc/resolver/k8s.test: permission denied")
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want the cleanup failure so uninstall exits non-zero", err)
	}
	if !strings.Contains(stderr.String(), "warning: could not remove resolver files") {
		t.Fatalf("stderr = %q, want a fallback-failure warning", stderr.String())
	}
}

func TestRemoveScopedResolverWarnsWhenUninstallFails(t *testing.T) {
	t.Parallel()

	client := &fakeDNSUninstaller{uninstal: errors.New("remove resolver file: permission denied")}
	var stderr bytes.Buffer
	command := cli{out: &bytes.Buffer{}, err: &stderr}
	fallbackCalls := 0
	err := command.removeScopedResolver(func() (dnsUninstaller, error) {
		return client, nil
	}, func() error {
		fallbackCalls++
		return errors.New("remove /etc/resolver/k8s.test: read-only file system")
	})
	if err == nil || !strings.Contains(err.Error(), "read-only file system") {
		t.Fatalf("error = %v, want the cleanup failure so uninstall exits non-zero", err)
	}
	if !client.closed {
		t.Fatal("helper connection was not closed")
	}
	if fallbackCalls != 1 {
		t.Fatalf("fallback calls = %d, want 1 (helper RPC failed, direct removal is the last chance)", fallbackCalls)
	}
	if !strings.Contains(stderr.String(), "warning: could not remove resolver files") {
		t.Fatalf("stderr = %q, want a resolver-removal warning", stderr.String())
	}
}

func TestRemoveScopedResolverFallsBackQuietlyWhenHelperUninstallFails(t *testing.T) {
	t.Parallel()

	client := &fakeDNSUninstaller{uninstal: errors.New("remove resolver file: permission denied")}
	var stderr bytes.Buffer
	command := cli{out: &bytes.Buffer{}, err: &stderr}
	if err := command.removeScopedResolver(func() (dnsUninstaller, error) {
		return client, nil
	}, func() error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty when the fallback succeeds", stderr.String())
	}
}

func TestRenderLaunchdPlist(t *testing.T) {
	t.Parallel()

	got, err := renderLaunchdPlist("/opt/Talos & Box/tbx-helper", uidPtr(501))
	if err != nil {
		t.Fatal(err)
	}
	const want = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n" +
		"<dict>\n" +
		"  <key>Label</key>\n" +
		"  <string>dev.talosbox.helper</string>\n" +
		"  <key>ProgramArguments</key>\n" +
		"  <array>\n" +
		"    <string>/opt/Talos &amp; Box/tbx-helper</string>\n" +
		"    <string>--allowed-uid</string>\n" +
		"    <string>501</string>\n" +
		"  </array>\n" +
		"  <key>RunAtLoad</key>\n" +
		"  <true/>\n" +
		"  <key>KeepAlive</key>\n" +
		"  <true/>\n" +
		"</dict>\n" +
		"</plist>\n"
	if string(got) != want {
		t.Fatalf("plist:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderLaunchdPlistWithoutAllowedUID(t *testing.T) {
	t.Parallel()

	got, err := renderLaunchdPlist("/usr/local/bin/tbx-helper", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "--allowed-uid") {
		t.Fatalf("plist includes --allowed-uid without an allowed uid:\n%s", got)
	}
	if !strings.Contains(string(got), "<string>/usr/local/bin/tbx-helper</string>\n  </array>") {
		t.Fatalf("plist program arguments malformed:\n%s", got)
	}
}

func TestRenderLaunchdPlistRejectsRelativePath(t *testing.T) {
	t.Parallel()

	if _, err := renderLaunchdPlist("tbx-helper", uidPtr(501)); err == nil {
		t.Fatal("renderLaunchdPlist accepted a relative path")
	}
}

func TestAllowedUIDFromSudoEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     string
		present   bool
		want      *uint32
		wantError string
	}{
		{name: "user uid", value: "501", present: true, want: uidPtr(501)},
		{name: "root uid", value: "0", present: true, want: uidPtr(0)},
		{name: "unset means root-only", want: nil},
		{name: "empty means root-only", present: true, want: nil},
		{name: "negative", value: "-1", present: true, wantError: "invalid SUDO_UID"},
		{name: "not numeric", value: "user", present: true, wantError: "invalid SUDO_UID"},
		{name: "too large", value: "4294967296", present: true, wantError: "invalid SUDO_UID"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := allowedUIDFromSudoEnv(func(string) (string, bool) {
				return test.value, test.present
			})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want containing %q", err, test.wantError)
				}
				if !strings.Contains(err.Error(), "sudo tbx system install") {
					t.Fatalf("error = %v, want reinstall guidance", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case test.want == nil && got != nil:
				t.Fatalf("allowed uid = %d, want nil", *got)
			case test.want != nil && got == nil:
				t.Fatalf("allowed uid = nil, want %d", *test.want)
			case test.want != nil && *got != *test.want:
				t.Fatalf("allowed uid = %d, want %d", *got, *test.want)
			}
		})
	}
}

func TestRemoveResolverFilesSweepsOnlyManagedFiles(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	shared := filepath.Join(directory, "k8s.test")
	if err := os.WriteFile(shared, []byte("nameserver 127.0.0.1\nport 5399\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "lab.internal"), []byte(resolverset.Content(5399)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "corp.vpn"), []byte("nameserver 10.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i, want := range []bool{true, false} { // second run: already gone, still succeeds
		removed, err := removeResolverFiles(shared)
		if err != nil {
			t.Fatal(err)
		}
		if removed != want {
			t.Fatalf("run %d: removed = %v, want %v", i+1, removed, want)
		}
	}
	if _, err := os.Stat(shared); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared resolver still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "lab.internal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed domain resolver still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "corp.vpn")); err != nil {
		t.Fatalf("unmanaged resolver file was touched: %v", err)
	}
}

func TestRemoveResolverFilesReportsUnreadableFiles(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	shared := filepath.Join(directory, "k8s.test")
	unreadable := filepath.Join(directory, "lab.internal")
	if err := os.WriteFile(unreadable, []byte(resolverset.Content(5399)), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := removeResolverFiles(shared)
	if err == nil || !strings.Contains(err.Error(), "read resolver file lab.internal") {
		t.Fatalf("error = %v, want an unreadable-file failure so the sweep does not pass silently", err)
	}
}
