package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const darwinTrustDriver = "darwin-login-keychain"

func unsupportedTrustPlatform(goos string) error {
	return fmt.Errorf("tbx trust is not supported on %s; install the cluster ingress CA manually", goos)
}

func (c cli) installDarwinTrust(name, caPath, fingerprint string) (*trustReceipt, error) {
	home, err := trustUserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	security, err := trustLookPath("/usr/bin/security")
	if err != nil {
		return nil, fmt.Errorf("find macOS security tool: %w", err)
	}
	if _, err := fmt.Fprintln(c.out, "macOS will show an interactive approval prompt for this trust change; approve it to continue."); err != nil {
		return nil, err
	}
	command := trustCommand{Name: security, Args: []string{"add-trusted-cert", "-r", "trustRoot", "-k", keychain, caPath}}
	if err := trustRunCommand(command, c.in, c.out, c.err); err != nil {
		return nil, fmt.Errorf("install ingress CA in macOS login keychain: %w", err)
	}
	return &trustReceipt{Cluster: name, Fingerprint: fingerprint, Store: "login keychain", Driver: darwinTrustDriver, Path: keychain}, nil
}

func (c cli) removeDarwinTrust(receipt trustReceipt) error {
	security, err := trustLookPath("/usr/bin/security")
	if err != nil {
		return fmt.Errorf("find macOS security tool: %w", err)
	}
	command := trustCommand{Name: security, Args: []string{"delete-certificate", "-Z", receipt.Fingerprint, receipt.Path}}
	if err := trustRunCommand(command, c.in, c.out, c.err); err != nil {
		return fmt.Errorf("remove ingress CA from macOS login keychain: %w", err)
	}
	return nil
}

func (c cli) darwinTrustEntryMatches(clusterName string, receipt trustReceipt, expectedFingerprint string) (bool, error) {
	security, err := trustLookPath("/usr/bin/security")
	if err != nil {
		return false, fmt.Errorf("find macOS security tool: %w", err)
	}
	command := trustCommand{
		Name: security,
		Args: []string{"find-certificate", "-Z", "-c", ingressCACommonName(clusterName), receipt.Path},
	}
	var stdout bytes.Buffer
	if err := trustRunCommand(command, nil, &stdout, io.Discard); err != nil {
		return false, nil
	}
	return outputContainsFingerprint(stdout.String(), expectedFingerprint), nil
}

const (
	linuxOSReleasePath = "/etc/os-release"
	linuxDebianDriver  = "debian"
	linuxFedoraDriver  = "fedora"
	linuxP11KitDriver  = "p11-kit"
	linuxDebianStore   = "/usr/local/share/ca-certificates"
	linuxFedoraStore   = "/etc/pki/ca-trust/source/anchors"
	linuxP11KitStore   = "/etc/ca-certificates/trust-source/anchors"
)

type linuxTrustDriver struct {
	Name     string
	StoreDir string
	Refresh  trustCommand
}

func (c cli) installLinuxTrust(name, caPath string, caPEM []byte, fingerprint string) (*trustReceipt, error) {
	driver, nixos, err := detectLinuxTrustDriver()
	if err != nil {
		return nil, err
	}
	if nixos {
		_, err := fmt.Fprintf(c.out, "NixOS manages CA trust declaratively; no host files were changed. Add this to configuration.nix, then rebuild the system:\n\nsecurity.pki.certificates = [\n  (builtins.readFile %q)\n];\n", caPath)
		return nil, err
	}
	filename := fmt.Sprintf("talosbox-%s-ingress-ca.crt", name)
	destination := filepath.Join(driver.StoreDir, filename)
	action := trustSystemAction{Operation: trustOperationInstall, Driver: driver.Name, Fingerprint: fingerprint, Destination: destination}
	if err := executeLinuxTrustAction(action, caPEM); err != nil {
		return nil, err
	}
	return &trustReceipt{Cluster: name, Fingerprint: fingerprint, Store: driver.StoreDir, Driver: driver.Name, Path: destination}, nil
}

func (c cli) removeLinuxTrust(receipt trustReceipt) error {
	action := trustSystemAction{Operation: trustOperationRemove, Driver: receipt.Driver, Fingerprint: receipt.Fingerprint, Destination: receipt.Path}
	return executeLinuxTrustAction(action, nil)
}

func executeLinuxTrustAction(action trustSystemAction, stdin []byte) error {
	if trustEUID() != 0 {
		return trustSudoReexec(action, stdin)
	}
	return applyLinuxTrustAction(action, bytesReader(stdin))
}

func (c cli) runTrustSystemAction(args []string) error {
	if len(args) != 4 {
		return errors.New("invalid internal trust-store action")
	}
	if trustEUID() != 0 {
		return errors.New("internal trust-store action requires root")
	}
	if trustGOOS() != "linux" {
		return errors.New("internal trust-store action is available only on Linux")
	}
	action := trustSystemAction{Operation: trustOperation(args[0]), Driver: args[1], Fingerprint: args[2], Destination: args[3]}
	return applyLinuxTrustAction(action, os.Stdin)
}

func applyLinuxTrustAction(action trustSystemAction, stdin io.Reader) error {
	driver, err := linuxTrustDriverByName(action.Driver, trustLookPath)
	if err != nil {
		return err
	}
	if filepath.Dir(action.Destination) != driver.StoreDir || !strings.HasPrefix(filepath.Base(action.Destination), "talosbox-") || !strings.HasSuffix(action.Destination, ".crt") {
		return errors.New("refuse trust-store mutation outside the selected talosbox anchor path")
	}
	switch action.Operation {
	case trustOperationInstall:
		certificatePEM, err := readValidatedTrustCertificatePEM(stdin, action.Fingerprint)
		if err != nil {
			return fmt.Errorf("validate %s trust anchor: %w", driver.Name, err)
		}
		if err := writeLinuxTrustAnchor(action.Destination, certificatePEM); err != nil {
			return fmt.Errorf("write %s trust anchor: %w", driver.Name, err)
		}
	case trustOperationRemove:
		if err := verifyTrustAnchorFingerprint(action.Destination, action.Fingerprint); err != nil {
			return err
		}
		if err := trustRemove(action.Destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s trust anchor: %w", driver.Name, err)
		}
	default:
		return fmt.Errorf("unknown trust-store operation %q", action.Operation)
	}
	if err := trustRunCommand(driver.Refresh, nil, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("refresh %s trust store: %w", driver.Name, err)
	}
	return nil
}

func bytesReader(data []byte) io.Reader {
	if len(data) == 0 {
		return nil
	}
	return bytes.NewReader(data)
}

func readValidatedTrustCertificatePEM(stdin io.Reader, expectedFingerprint string) ([]byte, error) {
	if stdin == nil {
		return nil, errors.New("trust install certificate input is empty")
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read certificate input: %w", err)
	}
	fingerprint, err := trustCertificateFingerprint(data)
	if err != nil {
		return nil, err
	}
	if expectedFingerprint == "" {
		return nil, errors.New("expected certificate fingerprint is empty")
	}
	if fingerprint != expectedFingerprint {
		return nil, fmt.Errorf("certificate fingerprint changed before the privileged trust-store update (expected %s, got %s)", expectedFingerprint, fingerprint)
	}
	return data, nil
}

func writeLinuxTrustAnchor(path string, certificatePEM []byte) error {
	file, err := trustOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		file, err = trustOpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	}
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(certificatePEM); err != nil {
		return err
	}
	return file.Chmod(0o644)
}

func verifyTrustAnchorFingerprint(path, expectedFingerprint string) error {
	if expectedFingerprint == "" {
		return errors.New("expected trust-anchor fingerprint is empty")
	}
	data, err := trustReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read trust anchor %s: %w", path, err)
	}
	fingerprint, fingerprintErr := trustCertificateFingerprint(data)
	if fingerprintErr == nil && fingerprint == expectedFingerprint {
		return nil
	}
	return fmt.Errorf("refuse to remove trust anchor %s: it changed since installation; remove it manually", path)
}

func linuxTrustEntryMatches(receipt trustReceipt, expectedFingerprint string) (bool, error) {
	if expectedFingerprint == "" {
		return false, errors.New("expected trust-anchor fingerprint is empty")
	}
	data, err := trustReadFile(receipt.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read trust anchor %s: %w", receipt.Path, err)
	}
	fingerprint, err := trustCertificateFingerprint(data)
	if err != nil {
		return false, nil
	}
	return fingerprint == expectedFingerprint, nil
}

func detectLinuxTrustDriver() (linuxTrustDriver, bool, error) {
	if _, err := trustStat("/etc/NIXOS"); err == nil {
		return linuxTrustDriver{}, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return linuxTrustDriver{}, false, fmt.Errorf("inspect NixOS marker: %w", err)
	}
	data, err := trustReadFile(linuxOSReleasePath)
	if err != nil {
		return linuxTrustDriver{}, false, fmt.Errorf("read %s: %w", linuxOSReleasePath, err)
	}
	return selectLinuxTrustDriver(data, trustLookPath)
}

func selectLinuxTrustDriver(osRelease []byte, lookup func(string) (string, error)) (linuxTrustDriver, bool, error) {
	fields := parseOSRelease(osRelease)
	id := strings.ToLower(fields["ID"])
	like := strings.Fields(strings.ToLower(fields["ID_LIKE"]))
	if id == "nixos" || containsString(like, "nixos") {
		return linuxTrustDriver{}, true, nil
	}
	switch {
	case id == "debian" || id == "ubuntu" || containsString(like, "debian") || containsString(like, "ubuntu"):
		driver, err := linuxTrustDriverByName(linuxDebianDriver, lookup)
		return driver, false, err
	case id == "fedora" || id == "rhel" || id == "centos" || containsString(like, "fedora") || containsString(like, "rhel") || containsString(like, "centos"):
		driver, err := linuxTrustDriverByName(linuxFedoraDriver, lookup)
		return driver, false, err
	case id == "arch" || containsString(like, "arch"):
		driver, err := linuxTrustDriverByName(linuxP11KitDriver, lookup)
		return driver, false, err
	default:
		return linuxTrustDriver{}, false, fmt.Errorf(
			"unsupported Linux trust store for distribution %q; supported anchor directories are %s, %s, and %s",
			id, linuxDebianStore, linuxFedoraStore, linuxP11KitStore)
	}
}

func linuxTrustDriverByName(name string, lookup func(string) (string, error)) (linuxTrustDriver, error) {
	var driver linuxTrustDriver
	var refreshName string
	switch name {
	case linuxDebianDriver:
		driver = linuxTrustDriver{Name: name, StoreDir: linuxDebianStore}
		refreshName = "update-ca-certificates"
	case linuxFedoraDriver:
		driver = linuxTrustDriver{Name: name, StoreDir: linuxFedoraStore}
		refreshName = "update-ca-trust"
	case linuxP11KitDriver:
		driver = linuxTrustDriver{Name: name, StoreDir: linuxP11KitStore}
		refreshName = "trust"
	default:
		return linuxTrustDriver{}, fmt.Errorf("unsupported Linux trust driver %q", name)
	}
	refresh, err := lookup(refreshName)
	if err != nil {
		return linuxTrustDriver{}, fmt.Errorf("find %s for %s trust store: %w", refreshName, name, err)
	}
	driver.Refresh = trustCommand{Name: refresh}
	if name == linuxFedoraDriver {
		driver.Refresh.Args = []string{"extract"}
	}
	if name == linuxP11KitDriver {
		driver.Refresh.Args = []string{"extract-compat"}
	}
	return driver, nil
}

func parseOSRelease(data []byte) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		fields[key] = strings.Trim(strings.TrimSpace(value), "\"'")
	}
	return fields
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func ingressCACommonName(clusterName string) string {
	return clusterName + " talos-box ingress CA"
}

var trustFingerprintPattern = regexp.MustCompile(`SHA-256 hash:\s*([0-9A-Fa-f]+)`)

func outputContainsFingerprint(output, expectedFingerprint string) bool {
	if expectedFingerprint == "" {
		return false
	}
	for _, match := range trustFingerprintPattern.FindAllStringSubmatch(output, -1) {
		if len(match) == 2 && strings.EqualFold(match[1], expectedFingerprint) {
			return true
		}
	}
	return false
}
