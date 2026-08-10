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

	tests := []struct {
		name     string
		euid     uint32
		override string
		want     string
		wantErr  string
	}{
		{name: "non-root runtime directory", euid: 501, want: "/run/user/501/tbx-helper.sock"},
		{name: "root system socket", euid: 0, want: systemHelperSocketPath},
		{name: "absolute override", euid: 501, override: "/tmp/custom.sock", want: "/tmp/custom.sock"},
		{name: "relative override rejected", euid: 501, override: "custom.sock", wantErr: helperSocketEnv},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := linuxClientSocketPath(test.euid, test.override)
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
