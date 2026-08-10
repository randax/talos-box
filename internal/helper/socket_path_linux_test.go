//go:build linux

package helper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxClientSocketPath(t *testing.T) {
	t.Parallel()
	originalStat := statSocketPath
	t.Cleanup(func() { statSocketPath = originalStat })
	statSocketPath = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	tests := []struct {
		name     string
		euid     uint32
		sudoUID  string
		override string
		want     string
		wantErr  string
	}{
		{name: "non-root runtime directory", euid: 501, want: "/run/user/501/tbx-helper.sock"},
		{name: "non-root ignores sudo uid", euid: 501, sudoUID: "502", want: "/run/user/501/tbx-helper.sock"},
		{name: "root system socket", euid: 0, want: systemHelperSocketPath},
		{name: "sudo root runtime directory", euid: 0, sudoUID: "501", want: "/run/user/501/tbx-helper.sock"},
		{name: "sudo root uid zero uses system socket", euid: 0, sudoUID: "0", want: systemHelperSocketPath},
		{name: "invalid sudo uid uses system socket", euid: 0, sudoUID: "invalid", want: systemHelperSocketPath},
		{name: "sudo root override wins", euid: 0, sudoUID: "501", override: "/tmp/custom.sock", want: "/tmp/custom.sock"},
		{name: "absolute override", euid: 501, override: "/tmp/custom.sock", want: "/tmp/custom.sock"},
		{name: "relative override rejected", euid: 501, override: "custom.sock", wantErr: helperSocketEnv},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := linuxClientSocketPath(test.euid, test.sudoUID, test.override)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("linuxClientSocketPath() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("linuxClientSocketPath() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLinuxSudoClientAndServerSocketPathsMatch(t *testing.T) {
	t.Parallel()
	originalStat := statSocketPath
	t.Cleanup(func() { statSocketPath = originalStat })
	statSocketPath = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	allowedUID := uint32(501)
	clientPath, err := linuxClientSocketPath(0, "501", "")
	if err != nil {
		t.Fatal(err)
	}
	serverPath, err := linuxServerSocketPath(0, &allowedUID, "")
	if err != nil {
		t.Fatal(err)
	}
	if clientPath != serverPath {
		t.Fatalf("sudo client/server paths = %q/%q, want them to match", clientPath, serverPath)
	}
}

func TestLinuxSocketOverrideIsSharedByClientAndServer(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "helper.sock")
	t.Setenv(helperSocketEnv, socketPath)

	allowedUID := uint32(os.Geteuid())
	serverPath, err := ServerSocketPath(&allowedUID)
	if err != nil {
		t.Fatal(err)
	}
	clientPath, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if serverPath != socketPath || clientPath != socketPath {
		t.Fatalf("server/client paths = %q/%q, want %q", serverPath, clientPath, socketPath)
	}

	listener, err := Listen(serverPath)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&allowedUID)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown()
		<-done
	})

	client, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxClientSocketPathPrefersSystemSocket(t *testing.T) {
	originalStat := statSocketPath
	t.Cleanup(func() { statSocketPath = originalStat })
	statSocketPath = func(path string) (os.FileInfo, error) {
		if path == systemHelperSocketPath {
			return os.Stat("/")
		}
		return nil, os.ErrNotExist
	}

	got, err := linuxClientSocketPath(501, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != systemHelperSocketPath {
		t.Fatalf("linuxClientSocketPath() = %q, want %q", got, systemHelperSocketPath)
	}
}

func TestLinuxClientSocketPathPreservesExistingUserHelper(t *testing.T) {
	originalStat := statSocketPath
	t.Cleanup(func() { statSocketPath = originalStat })
	statSocketPath = func(path string) (os.FileInfo, error) {
		if path == systemHelperSocketPath || path == linuxUserSocketPath(501) {
			return os.Stat("/")
		}
		return nil, os.ErrNotExist
	}

	got, err := linuxClientSocketPath(501, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := linuxUserSocketPath(501); got != want {
		t.Fatalf("linuxClientSocketPath() = %q, want existing user helper %q", got, want)
	}
}

func TestLinuxServerSocketPathMatchesAuthorizedClient(t *testing.T) {
	t.Parallel()

	uid501 := uint32(501)
	tests := []struct {
		name       string
		euid       uint32
		allowedUID *uint32
		override   string
		want       string
	}{
		{name: "capability helper", euid: 501, allowedUID: &uid501, want: "/run/user/501/tbx-helper.sock"},
		{name: "sudo root helper", euid: 0, allowedUID: &uid501, want: "/run/user/501/tbx-helper.sock"},
		{name: "root-only helper", euid: 0, want: systemHelperSocketPath},
		{name: "override", euid: 0, allowedUID: &uid501, override: "/tmp/custom.sock", want: "/tmp/custom.sock"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := linuxServerSocketPath(test.euid, test.allowedUID, test.override)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("linuxServerSocketPath() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestValidateLinuxSocketPathExplainsMissingRuntimeDirectory(t *testing.T) {
	t.Parallel()

	path := linuxUserSocketPath(^uint32(0))
	_, err := validateLinuxSocketPath(path, "", nil)
	if err == nil || !strings.Contains(err.Error(), helperSocketEnv) || !strings.Contains(err.Error(), "/run/user/") {
		t.Fatalf("validateLinuxSocketPath() error = %v, want runtime-directory error with %s guidance", err, helperSocketEnv)
	}
}
