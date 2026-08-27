package linuxassets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelperSocketAsset(t *testing.T) {
	content := readAsset(t, "usr/lib/systemd/system/tbx-helper.socket")
	for _, want := range []string{
		"ListenStream=/var/run/tbx-helper.sock",
		"SocketMode=0660",
		"SocketGroup=tbx",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("helper socket asset missing %q", want)
		}
	}
}

func TestHelperServiceAsset(t *testing.T) {
	content := readAsset(t, "usr/lib/systemd/system/tbx-helper.service")
	for _, want := range []string{
		"User=tbx",
		"Group=tbx",
		"AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW",
		"CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK",
		"DeviceAllow=/dev/net/tun",
		"DynamicUser=no",
		"PrivateNetwork=no",
		"ExecStart=/usr/bin/tbx-helper",
		// The helper keeps the reservations tbxd pushes here; without a state
		// directory a restart forgets them and serves no DHCP until the next
		// sync, and ProtectSystem=strict leaves nowhere else writable.
		"StateDirectory=tbx",
		"StateDirectoryMode=0700",
		// A hand-started service must still inherit the socket's descriptor.
		"Requires=tbx-helper.socket",
		"After=tbx-helper.socket",
		"Environment=TBX_HELPER_SOCKET=/var/run/tbx-helper.sock",
		// The helper never reads a user home; the unit enforces it.
		"ProtectHome=yes",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("helper service asset missing %q", want)
		}
	}
	if strings.Contains(content, "ProtectKernelTunables=yes") {
		t.Fatal("helper service asset blocks required sysctl writes")
	}
}

func TestTbxdUserUnits(t *testing.T) {
	socket := readAsset(t, "usr/lib/systemd/user/tbxd.socket")
	for _, want := range []string{
		"ListenStream=%h/.talosbox/tbxd.sock",
		"SocketMode=0600",
	} {
		if !strings.Contains(socket, want) {
			t.Fatalf("tbxd socket asset missing %q", want)
		}
	}

	service := readAsset(t, "usr/lib/systemd/user/tbxd.service")
	if !strings.Contains(service, "ExecStart=/usr/bin/tbxd") {
		t.Fatalf("tbxd service asset missing ExecStart")
	}
}

func TestSysusersAndPolkitAssets(t *testing.T) {
	sysusers := readAsset(t, "usr/lib/sysusers.d/talos-box.conf")
	for _, want := range []string{
		"g tbx",
		"u tbx",
	} {
		if !strings.Contains(sysusers, want) {
			t.Fatalf("sysusers asset missing %q", want)
		}
	}

	polkit := readAsset(t, "usr/share/polkit-1/rules.d/90-talos-box-resolved.rules")
	for _, want := range []string{
		"subject.user == \"tbx\"",
		"org.freedesktop.resolve1.set-dns-servers",
		"org.freedesktop.resolve1.set-domains",
		"org.freedesktop.resolve1.revert",
	} {
		if !strings.Contains(polkit, want) {
			t.Fatalf("polkit asset missing %q", want)
		}
	}
	if strings.Contains(polkit, "register-service") {
		t.Fatal("polkit asset authorizes DNS-SD registration")
	}
}

func readAsset(t *testing.T, relative string) string {
	t.Helper()
	path := filepath.Join("..", "linux", relative)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
