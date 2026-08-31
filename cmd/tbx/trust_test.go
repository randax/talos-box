package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSelectLinuxTrustDriver(t *testing.T) {
	toolPaths := map[string]string{
		"update-ca-certificates": "/test-bin/update-ca-certificates",
		"update-ca-trust":        "/test-bin/update-ca-trust",
		"trust":                  "/test-bin/trust",
	}
	lookup := func(name string) (string, error) {
		if path, ok := toolPaths[name]; ok {
			return path, nil
		}
		return "", os.ErrNotExist
	}
	tests := []struct {
		name      string
		osRelease string
		wantName  string
		wantTool  string
	}{
		{name: "Debian", osRelease: "ID=debian\n", wantName: "debian", wantTool: "update-ca-certificates"},
		{name: "Ubuntu", osRelease: "ID=ubuntu\nID_LIKE=debian\n", wantName: "debian", wantTool: "update-ca-certificates"},
		{name: "Fedora", osRelease: "ID=fedora\n", wantName: "fedora", wantTool: "update-ca-trust"},
		{name: "RHEL family", osRelease: "ID=rocky\nID_LIKE=\"rhel fedora\"\n", wantName: "fedora", wantTool: "update-ca-trust"},
		{name: "Arch", osRelease: "ID=arch\n", wantName: "p11-kit", wantTool: "trust"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver, nixos, err := selectLinuxTrustDriver([]byte(test.osRelease), lookup)
			if err != nil {
				t.Fatal(err)
			}
			if nixos {
				t.Fatal("nixos = true")
			}
			if driver.Name != test.wantName {
				t.Fatalf("driver = %q, want %q", driver.Name, test.wantName)
			}
			if driver.Refresh.Name != toolPaths[test.wantTool] {
				t.Fatalf("refresh command = %q, want injected %q", driver.Refresh.Name, toolPaths[test.wantTool])
			}
		})
	}
}

func TestApplyLinuxTrustActionUsesSelectedDriverCommands(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	toolPaths := map[string]string{
		"update-ca-certificates": "/test-bin/update-ca-certificates",
		"update-ca-trust":        "/test-bin/update-ca-trust",
		"trust":                  "/test-bin/trust",
	}
	trustLookPath = func(name string) (string, error) { return toolPaths[name], nil }
	for _, driverName := range []string{linuxDebianDriver, linuxFedoraDriver, linuxP11KitDriver} {
		t.Run(driverName, func(t *testing.T) {
			driver, err := linuxTrustDriverByName(driverName, trustLookPath)
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(driver.StoreDir, "talosbox-demo-ingress-ca.crt")
			writtenPath := filepath.Join(t.TempDir(), "anchor.crt")
			var openCalls []struct {
				path  string
				flags int
				perm  os.FileMode
			}
			trustOpenFile = func(path string, flags int, perm os.FileMode) (*os.File, error) {
				openCalls = append(openCalls, struct {
					path  string
					flags int
					perm  os.FileMode
				}{path: path, flags: flags, perm: perm})
				return os.OpenFile(writtenPath, flags, perm)
			}
			var commands []trustCommand
			trustRunCommand = func(command trustCommand, _ io.Reader, _, _ io.Writer) error {
				commands = append(commands, command)
				return nil
			}
			if err := applyLinuxTrustAction(trustSystemAction{
				Operation: trustOperationInstall, Driver: driverName, Fingerprint: fixture.fingerprint, Destination: destination,
			}, bytes.NewReader(fixture.certPEM)); err != nil {
				t.Fatal(err)
			}
			if len(openCalls) != 1 || openCalls[0].path != destination || openCalls[0].perm != 0o644 || openCalls[0].flags != os.O_WRONLY|os.O_CREATE|os.O_EXCL {
				t.Fatalf("open calls = %+v", openCalls)
			}
			if len(commands) != 1 || !reflect.DeepEqual(commands[0], driver.Refresh) {
				t.Fatalf("commands = %+v, want injected refresh %+v", commands, driver.Refresh)
			}
			if got, err := os.ReadFile(writtenPath); err != nil {
				t.Fatal(err)
			} else if !bytes.Equal(got, fixture.certPEM) {
				t.Fatalf("written PEM = %q, want %q", got, fixture.certPEM)
			}
		})
	}
}

func TestTrustInstallAndRemoveReceiptLifecycleAfterClusterDeletion(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	fixture.osRelease = []byte("ID=ubuntu\n")
	var actions []trustSystemAction
	var installPEM [][]byte
	trustSudoReexec = func(action trustSystemAction, stdin []byte) error {
		actions = append(actions, action)
		installPEM = append(installPEM, append([]byte(nil), stdin...))
		return nil
	}

	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	receipt, err := readTrustReceipt(fixture.cluster)
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if receipt.Cluster != fixture.cluster || receipt.Fingerprint == "" || receipt.Driver != "debian" || receipt.Path == "" || receipt.InstalledAt.IsZero() {
		t.Fatalf("receipt = %+v", receipt)
	}
	receiptPath, err := trustReceiptPath(fixture.cluster)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(receiptPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt mode = %o, want 600", info.Mode().Perm())
	}
	if len(actions) != 1 || actions[0].Operation != trustOperationInstall || actions[0].Driver != receipt.Driver || actions[0].Destination != receipt.Path || actions[0].Fingerprint != receipt.Fingerprint {
		t.Fatalf("install actions = %+v, receipt = %+v", actions, receipt)
	}
	if len(installPEM) != 1 || !bytes.Equal(installPEM[0], fixture.certPEM) {
		t.Fatalf("install stdin = %q, want CA PEM", installPEM)
	}

	if err := os.Remove(fixture.certPath); err != nil {
		t.Fatal(err)
	}
	if err := command.run([]string{"trust", "remove", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[1].Operation != trustOperationRemove || actions[1].Destination != receipt.Path || actions[1].Fingerprint != receipt.Fingerprint {
		t.Fatalf("actions after remove = %+v", actions)
	}
	if len(installPEM) != 2 || len(installPEM[1]) != 0 {
		t.Fatalf("remove unexpectedly passed stdin = %q", installPEM)
	}
	if _, err := readTrustReceipt(fixture.cluster); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt remains after removal: %v", err)
	}
}

func TestDarwinTrustCancellationReturnsFailureWithoutReceipt(t *testing.T) {
	fixture := newTrustFixture(t, "darwin")
	cancelled := errors.New("approval cancelled")
	securityPath := "/test-bin/security"
	trustLookPath = func(name string) (string, error) { return securityPath, nil }
	var commands []trustCommand
	trustRunCommand = func(command trustCommand, _ io.Reader, _, _ io.Writer) error {
		commands = append(commands, command)
		return cancelled
	}

	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	err := command.run([]string{"trust", "install", fixture.cluster})
	if !errors.Is(err, cancelled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if len(commands) != 1 || commands[0].Name != securityPath {
		t.Fatalf("commands = %+v, want injected security executable", commands)
	}
	if !strings.Contains(fixture.stdout.String(), "interactive approval prompt") {
		t.Fatalf("prompt warning was not printed before execution:\n%s", fixture.stdout.String())
	}
	if _, err := readTrustReceipt(fixture.cluster); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt written after cancellation: %v", err)
	}
}

func TestNixOSTrustInstallPrintsDeclarationWithoutMutation(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	fixture.osRelease = []byte("ID=nixos\n")
	mutated := false
	trustStat = func(path string) (os.FileInfo, error) {
		if path == "/etc/NIXOS" {
			return regularFileInfo{name: "NIXOS"}, nil
		}
		return os.Stat(path)
	}
	trustSudoReexec = func(trustSystemAction, []byte) error { mutated = true; return nil }
	trustRunCommand = func(trustCommand, io.Reader, io.Writer, io.Writer) error { mutated = true; return nil }

	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if mutated {
		t.Fatal("NixOS install attempted a command or sudo mutation")
	}
	for _, want := range []string{"security.pki.certificates", "builtins.readFile", fixture.certPath, "no host files were changed"} {
		if !strings.Contains(fixture.stdout.String(), want) {
			t.Fatalf("NixOS instructions missing %q:\n%s", want, fixture.stdout.String())
		}
	}
	if _, err := readTrustReceipt(fixture.cluster); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NixOS instruction wrote a receipt: %v", err)
	}
}

func TestTrustInstallIsIdempotent(t *testing.T) {
	fixture := newTrustFixture(t, "darwin")
	trustLookPath = func(name string) (string, error) { return "/test-bin/security", nil }
	runs := 0
	trustRunCommand = func(command trustCommand, _ io.Reader, stdout, _ io.Writer) error {
		runs++
		switch {
		case reflect.DeepEqual(command.Args, []string{"add-trusted-cert", "-r", "trustRoot", "-k", filepath.Join(fixture.home, "Library", "Keychains", "login.keychain-db"), fixture.certPath}):
			return nil
		case reflect.DeepEqual(command.Args, []string{"find-certificate", "-Z", "-c", "demo talos-box ingress CA", filepath.Join(fixture.home, "Library", "Keychains", "login.keychain-db")}):
			_, err := io.WriteString(stdout, "SHA-256 hash: "+fixture.fingerprint+"\n")
			return err
		default:
			t.Fatalf("unexpected trust command: %+v", command)
			return nil
		}
	}
	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}

	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Fatalf("security command ran %d times, want add+validate", runs)
	}
	if !strings.Contains(fixture.stdout.String(), "already installed") {
		t.Fatalf("idempotent result is unclear:\n%s", fixture.stdout.String())
	}
}

func TestTrustInstallReinstallsLinuxAnchorRemovedOutOfBand(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	fixture.osRelease = []byte("ID=ubuntu\n")
	var actions []trustSystemAction
	trustSudoReexec = func(action trustSystemAction, stdin []byte) error {
		actions = append(actions, action)
		if action.Operation != trustOperationInstall {
			t.Fatalf("unexpected Linux trust action: %+v", action)
		}
		if !bytes.Equal(stdin, fixture.certPEM) {
			t.Fatalf("install stdin = %q, want CA PEM", stdin)
		}
		return nil
	}
	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	receipt, err := readTrustReceipt(fixture.cluster)
	if err != nil {
		t.Fatal(err)
	}
	fixture.stdout.Reset()
	trustReadFile = func(path string) ([]byte, error) {
		if path == linuxOSReleasePath {
			return fixture.osRelease, nil
		}
		if path == receipt.Path {
			return nil, os.ErrNotExist
		}
		return os.ReadFile(path)
	}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("Linux install actions = %+v", actions)
	}
	if strings.Contains(fixture.stdout.String(), "already installed") {
		t.Fatalf("removed anchor incorrectly short-circuited:\n%s", fixture.stdout.String())
	}
}

func TestTrustInstallReinstallsLinuxAnchorSwappedOutOfBand(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	fixture.osRelease = []byte("ID=ubuntu\n")
	var actions []trustSystemAction
	trustSudoReexec = func(action trustSystemAction, stdin []byte) error {
		actions = append(actions, action)
		if action.Operation != trustOperationInstall {
			t.Fatalf("unexpected Linux trust action: %+v", action)
		}
		if !bytes.Equal(stdin, fixture.certPEM) {
			t.Fatalf("install stdin = %q, want CA PEM", stdin)
		}
		return nil
	}
	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	receipt, err := readTrustReceipt(fixture.cluster)
	if err != nil {
		t.Fatal(err)
	}
	otherPEM := testTrustCertificate(t)
	fixture.stdout.Reset()
	trustReadFile = func(path string) ([]byte, error) {
		if path == linuxOSReleasePath {
			return fixture.osRelease, nil
		}
		if path == receipt.Path {
			return otherPEM, nil
		}
		return os.ReadFile(path)
	}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("Linux install actions = %+v", actions)
	}
	if strings.Contains(fixture.stdout.String(), "already installed") {
		t.Fatalf("swapped anchor incorrectly short-circuited:\n%s", fixture.stdout.String())
	}
}

func TestTrustInstallReinstallsDarwinEntryRemovedOutOfBand(t *testing.T) {
	fixture := newTrustFixture(t, "darwin")
	securityPath := "/test-bin/security"
	trustLookPath = func(string) (string, error) { return securityPath, nil }
	var commands []trustCommand
	installedFingerprint := ""
	trustRunCommand = func(command trustCommand, _ io.Reader, stdout, stderr io.Writer) error {
		commands = append(commands, command)
		switch command.Args[0] {
		case "add-trusted-cert":
			installedFingerprint = fixture.fingerprint
			return nil
		case "find-certificate":
			if installedFingerprint == "" {
				return errors.New("certificate not found")
			}
			_, err := io.WriteString(stdout, "SHA-256 hash: "+installedFingerprint+"\n")
			return err
		default:
			t.Fatalf("unexpected security command: %+v", command)
			_, _ = io.WriteString(stderr, "unexpected command")
			return nil
		}
	}
	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	installedFingerprint = ""
	fixture.stdout.Reset()
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 3 {
		t.Fatalf("security commands = %+v", commands)
	}
	if strings.Contains(fixture.stdout.String(), "already installed") {
		t.Fatalf("removed keychain entry incorrectly short-circuited:\n%s", fixture.stdout.String())
	}
}

func TestTrustInstallReinstallsDarwinEntrySwappedOutOfBand(t *testing.T) {
	fixture := newTrustFixture(t, "darwin")
	securityPath := "/test-bin/security"
	trustLookPath = func(string) (string, error) { return securityPath, nil }
	var commands []trustCommand
	otherPEM := testTrustCertificate(t)
	otherFingerprint, err := trustCertificateFingerprint(otherPEM)
	if err != nil {
		t.Fatal(err)
	}
	installedFingerprint := ""
	trustRunCommand = func(command trustCommand, _ io.Reader, stdout, stderr io.Writer) error {
		commands = append(commands, command)
		switch command.Args[0] {
		case "add-trusted-cert":
			installedFingerprint = fixture.fingerprint
			return nil
		case "find-certificate":
			if installedFingerprint == "" {
				return errors.New("certificate not found")
			}
			_, err := io.WriteString(stdout, "SHA-256 hash: "+installedFingerprint+"\n")
			return err
		default:
			t.Fatalf("unexpected security command: %+v", command)
			_, _ = io.WriteString(stderr, "unexpected command")
			return nil
		}
	}
	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	installedFingerprint = otherFingerprint
	fixture.stdout.Reset()
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 3 {
		t.Fatalf("security commands = %+v", commands)
	}
	if strings.Contains(fixture.stdout.String(), "already installed") {
		t.Fatalf("swapped keychain entry incorrectly short-circuited:\n%s", fixture.stdout.String())
	}
}

func TestTrustInstallRefusesReplacingDifferentRecordedCA(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	fixture.osRelease = []byte("ID=ubuntu\n")
	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	otherPEM := testTrustCertificate(t)
	if err := os.WriteFile(fixture.certPath, otherPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	err := command.run([]string{"trust", "install", fixture.cluster})
	if err == nil || !strings.Contains(err.Error(), "different trusted CA recorded") {
		t.Fatalf("second install error = %v, want recorded-CA refusal", err)
	}
}

func TestTrustRemoveUninstalledIsCleanNoop(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	called := false
	trustSudoReexec = func(trustSystemAction, []byte) error { called = true; return nil }
	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	if err := command.run([]string{"trust", "remove", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("remove without a receipt attempted a privileged mutation")
	}
	if !strings.Contains(fixture.stdout.String(), "not installed") {
		t.Fatalf("no-op result is unclear:\n%s", fixture.stdout.String())
	}
}

func TestTrustCommandRegistrationAndHelp(t *testing.T) {
	if got := groupUsages["trust"]; got == "" {
		t.Fatal("trust group usage is not registered")
	}
	var stdout bytes.Buffer
	command := cli{out: &stdout, err: io.Discard, daemon: nil}
	command.printHelp(&stdout)
	if !strings.Contains(stdout.String(), "trust install|remove <cluster>") {
		t.Fatalf("top-level help omits trust command:\n%s", stdout.String())
	}
	if err := command.run([]string{"trust"}); err == nil || err.Error() != groupUsages["trust"] {
		t.Fatalf("bare trust error = %v, want group usage", err)
	}
}

type trustFixture struct {
	cluster     string
	home        string
	certPath    string
	certPEM     []byte
	fingerprint string
	osRelease   []byte
	stdout      bytes.Buffer
	stderr      bytes.Buffer
}

func newTrustFixture(t *testing.T, goos string) *trustFixture {
	t.Helper()
	fixture := &trustFixture{cluster: "demo", home: t.TempDir()}
	fixture.certPath = filepath.Join(fixture.home, ".talosbox", "clusters", fixture.cluster, "ingress-ca.crt")
	if err := os.MkdirAll(filepath.Dir(fixture.certPath), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.certPEM = testTrustCertificate(t)
	fingerprint, err := trustCertificateFingerprint(fixture.certPEM)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fingerprint = fingerprint
	if err := os.WriteFile(fixture.certPath, fixture.certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	originalGOOS := trustGOOS
	originalHome := trustUserHomeDir
	originalCAPath := trustIngressCAPath
	originalRead := trustReadFile
	originalWrite := trustWriteFile
	originalMkdir := trustMkdirAll
	originalRemove := trustRemove
	originalRename := trustRename
	originalStat := trustStat
	originalOpen := trustOpenFile
	originalLookup := trustLookPath
	originalEUID := trustEUID
	originalNow := trustNow
	originalRun := trustRunCommand
	originalSudo := trustSudoReexec
	t.Cleanup(func() {
		trustGOOS = originalGOOS
		trustUserHomeDir = originalHome
		trustIngressCAPath = originalCAPath
		trustReadFile = originalRead
		trustWriteFile = originalWrite
		trustMkdirAll = originalMkdir
		trustRemove = originalRemove
		trustRename = originalRename
		trustStat = originalStat
		trustOpenFile = originalOpen
		trustLookPath = originalLookup
		trustEUID = originalEUID
		trustNow = originalNow
		trustRunCommand = originalRun
		trustSudoReexec = originalSudo
	})

	trustGOOS = func() string { return goos }
	trustUserHomeDir = func() (string, error) { return fixture.home, nil }
	trustIngressCAPath = func(name string) (string, error) {
		if name != fixture.cluster {
			t.Fatalf("CA path requested for %q", name)
		}
		return fixture.certPath, nil
	}
	trustReadFile = func(path string) ([]byte, error) {
		if path == linuxOSReleasePath {
			return fixture.osRelease, nil
		}
		return os.ReadFile(path)
	}
	trustWriteFile = os.WriteFile
	trustMkdirAll = os.MkdirAll
	trustRemove = os.Remove
	trustRename = os.Rename
	trustStat = func(path string) (os.FileInfo, error) {
		if path == "/etc/NIXOS" {
			return nil, os.ErrNotExist
		}
		return os.Stat(path)
	}
	trustOpenFile = os.OpenFile
	trustLookPath = func(name string) (string, error) { return filepath.Join("/test-bin", name), nil }
	trustEUID = func() int { return 1000 }
	trustNow = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	trustRunCommand = func(trustCommand, io.Reader, io.Writer, io.Writer) error { return nil }
	trustSudoReexec = func(trustSystemAction, []byte) error { return nil }
	return fixture
}

func testTrustCertificate(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ingress CA"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(0, 0).AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestDarwinRemoveUsesRecordedFingerprint(t *testing.T) {
	fixture := newTrustFixture(t, "darwin")
	securityPath := "/test-bin/security"
	trustLookPath = func(string) (string, error) { return securityPath, nil }
	var commands []trustCommand
	trustRunCommand = func(command trustCommand, _ io.Reader, _, _ io.Writer) error {
		commands = append(commands, command)
		return nil
	}
	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	receipt, err := readTrustReceipt(fixture.cluster)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.run([]string{"trust", "remove", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 {
		t.Fatalf("commands = %+v", commands)
	}
	wantDelete := trustCommand{Name: securityPath, Args: []string{"delete-certificate", "-Z", receipt.Fingerprint, receipt.Path}}
	if !reflect.DeepEqual(commands[1], wantDelete) {
		t.Fatalf("delete command = %+v, want %+v", commands[1], wantDelete)
	}
}

type regularFileInfo struct{ name string }

func (i regularFileInfo) Name() string     { return i.name }
func (regularFileInfo) Size() int64        { return 0 }
func (regularFileInfo) Mode() os.FileMode  { return 0o600 }
func (regularFileInfo) ModTime() time.Time { return time.Time{} }
func (regularFileInfo) IsDir() bool        { return false }
func (regularFileInfo) Sys() any           { return nil }

func TestApplyLinuxTrustActionRejectsCertificateFingerprintMismatch(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	trustLookPath = func(name string) (string, error) { return filepath.Join("/test-bin", name), nil }
	trustOpenFile = func(string, int, os.FileMode) (*os.File, error) {
		t.Fatal("mismatched certificate should not be written")
		return nil, nil
	}
	err := applyLinuxTrustAction(trustSystemAction{
		Operation:   trustOperationInstall,
		Driver:      linuxDebianDriver,
		Fingerprint: strings.Repeat("A", len(fixture.fingerprint)),
		Destination: filepath.Join(linuxDebianStore, "talosbox-demo-ingress-ca.crt"),
	}, bytes.NewReader(fixture.certPEM))
	if err == nil || !strings.Contains(err.Error(), "fingerprint changed") {
		t.Fatalf("applyLinuxTrustAction() error = %v, want fingerprint mismatch", err)
	}
}

func TestTrustSystemInstallReadsValidatedCertificateFromStdin(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	trustEUID = func() int { return 0 }
	writtenPath := filepath.Join(t.TempDir(), "anchor.crt")
	trustOpenFile = func(path string, flags int, perm os.FileMode) (*os.File, error) {
		if path != filepath.Join(linuxDebianStore, "talosbox-demo-ingress-ca.crt") {
			t.Fatalf("destination path = %q", path)
		}
		return os.OpenFile(writtenPath, flags, perm)
	}
	var refreshed []trustCommand
	trustRunCommand = func(command trustCommand, _ io.Reader, _, _ io.Writer) error {
		refreshed = append(refreshed, command)
		return nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(fixture.certPEM); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() { os.Stdin = originalStdin })
	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}
	if err := command.run([]string{"trust", "_system", "install", linuxDebianDriver, fixture.fingerprint, filepath.Join(linuxDebianStore, "talosbox-demo-ingress-ca.crt")}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(writtenPath); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, fixture.certPEM) {
		t.Fatalf("written PEM = %q, want %q", got, fixture.certPEM)
	}
	if len(refreshed) != 1 || refreshed[0].Name != "/test-bin/update-ca-certificates" {
		t.Fatalf("refresh commands = %+v", refreshed)
	}
}

func TestApplyLinuxTrustActionRefusesRemovingChangedAnchor(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	otherPEM := testTrustCertificate(t)
	trustReadFile = func(path string) ([]byte, error) {
		if path == filepath.Join(linuxDebianStore, "talosbox-demo-ingress-ca.crt") {
			return otherPEM, nil
		}
		return os.ReadFile(path)
	}
	trustRemove = func(string) error {
		t.Fatal("changed anchor should not be removed")
		return nil
	}
	trustRunCommand = func(trustCommand, io.Reader, io.Writer, io.Writer) error {
		t.Fatal("changed anchor should not refresh trust store")
		return nil
	}
	err := applyLinuxTrustAction(trustSystemAction{
		Operation:   trustOperationRemove,
		Driver:      linuxDebianDriver,
		Fingerprint: fixture.fingerprint,
		Destination: filepath.Join(linuxDebianStore, "talosbox-demo-ingress-ca.crt"),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "remove it manually") {
		t.Fatalf("applyLinuxTrustAction() error = %v, want manual removal refusal", err)
	}
}

func TestSelectLinuxTrustDriverRejectsUnknownTrustStoreEvenWhenTrustExists(t *testing.T) {
	lookup := func(name string) (string, error) {
		return filepath.Join("/test-bin", name), nil
	}
	_, _, err := selectLinuxTrustDriver([]byte("ID=opensuse\n"), lookup)
	if err == nil {
		t.Fatal("unknown distro unexpectedly mapped to a trust driver")
	}
	for _, want := range []string{"unsupported Linux trust store", linuxDebianStore, linuxFedoraStore, linuxP11KitStore} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}
