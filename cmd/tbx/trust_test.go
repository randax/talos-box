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
		"install":                "/test-bin/install",
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
			var commands []trustCommand
			trustRunCommand = func(command trustCommand, _ io.Reader, _, _ io.Writer) error {
				commands = append(commands, command)
				return nil
			}
			if err := applyLinuxTrustAction(trustSystemAction{
				Operation: trustOperationInstall, Driver: driverName, Source: fixture.certPath, Destination: destination,
			}); err != nil {
				t.Fatal(err)
			}
			wantInstall := trustCommand{Name: toolPaths["install"], Args: []string{"-m", "0644", fixture.certPath, destination}}
			if len(commands) != 2 || !reflect.DeepEqual(commands[0], wantInstall) || !reflect.DeepEqual(commands[1], driver.Refresh) {
				t.Fatalf("commands = %+v, want install %+v then injected refresh %+v", commands, wantInstall, driver.Refresh)
			}
		})
	}
}

func TestTrustInstallAndRemoveReceiptLifecycleAfterClusterDeletion(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	fixture.osRelease = []byte("ID=ubuntu\n")
	var actions []trustSystemAction
	trustSudoReexec = func(action trustSystemAction) error {
		actions = append(actions, action)
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
	if len(actions) != 1 || actions[0].Operation != trustOperationInstall || actions[0].Driver != receipt.Driver || actions[0].Destination != receipt.Path {
		t.Fatalf("install actions = %+v, receipt = %+v", actions, receipt)
	}

	if err := os.Remove(fixture.certPath); err != nil {
		t.Fatal(err)
	}
	if err := command.run([]string{"trust", "remove", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[1].Operation != trustOperationRemove || actions[1].Destination != receipt.Path {
		t.Fatalf("actions after remove = %+v", actions)
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
	trustSudoReexec = func(trustSystemAction) error { mutated = true; return nil }
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
	trustRunCommand = func(trustCommand, io.Reader, io.Writer, io.Writer) error { runs++; return nil }
	command := cli{out: &fixture.stdout, err: &fixture.stderr, daemon: nil}

	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if err := command.run([]string{"trust", "install", fixture.cluster}); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("security command ran %d times, want one", runs)
	}
	if !strings.Contains(fixture.stdout.String(), "already installed") {
		t.Fatalf("idempotent result is unclear:\n%s", fixture.stdout.String())
	}
}

func TestTrustRemoveUninstalledIsCleanNoop(t *testing.T) {
	fixture := newTrustFixture(t, "linux")
	called := false
	trustSudoReexec = func(trustSystemAction) error { called = true; return nil }
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
	cluster   string
	home      string
	certPath  string
	osRelease []byte
	stdout    bytes.Buffer
	stderr    bytes.Buffer
}

func newTrustFixture(t *testing.T, goos string) *trustFixture {
	t.Helper()
	fixture := &trustFixture{cluster: "demo", home: t.TempDir()}
	fixture.certPath = filepath.Join(fixture.home, ".talosbox", "clusters", fixture.cluster, "ingress-ca.crt")
	if err := os.MkdirAll(filepath.Dir(fixture.certPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.certPath, testTrustCertificate(t), 0o600); err != nil {
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
	trustStat = os.Stat
	trustLookPath = func(name string) (string, error) { return filepath.Join("/test-bin", name), nil }
	trustEUID = func() int { return 1000 }
	trustNow = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	trustRunCommand = func(trustCommand, io.Reader, io.Writer, io.Writer) error { return nil }
	trustSudoReexec = func(trustSystemAction) error { return nil }
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
