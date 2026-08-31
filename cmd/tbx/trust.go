package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

const trustReceiptDirectory = "trust"

type trustReceipt struct {
	Cluster     string    `json:"cluster"`
	Fingerprint string    `json:"fingerprint"`
	Store       string    `json:"store"`
	Driver      string    `json:"driver"`
	Path        string    `json:"path"`
	Pending     bool      `json:"pending,omitempty"`
	InstalledAt time.Time `json:"installedAt"`
}

type trustCommand struct {
	Name string
	Args []string
}

type trustOperation string

const (
	trustOperationInstall trustOperation = "install"
	trustOperationRemove  trustOperation = "remove"
)

// trustSystemAction is the narrow payload re-executed as root on Linux. It
// contains only the conventional store mutation; receipt I/O remains in the
// original unprivileged process.
type trustSystemAction struct {
	Operation   trustOperation
	Driver      string
	Fingerprint string
	Destination string
}

func (c cli) runTrust(args []string) error {
	if len(args) == 0 {
		return errors.New(groupUsages["trust"])
	}
	switch args[0] {
	case "install":
		if len(args) != 2 {
			return errors.New("usage: tbx trust install <cluster>")
		}
		return c.installTrust(args[1])
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: tbx trust remove <cluster>")
		}
		return c.removeTrust(args[1])
	case "_system":
		return c.runTrustSystemAction(args[1:])
	default:
		return unknownVerbError("trust", args[0])
	}
}

func (c cli) installTrust(name string) error {
	if err := cluster.ValidateName(name); err != nil {
		return err
	}
	caPath, err := trustIngressCAPath(name)
	if err != nil {
		return err
	}
	caPEM, err := trustReadFile(caPath)
	if err != nil {
		return fmt.Errorf("read ingress CA for cluster %q (provision Cilium ingress first): %w", name, err)
	}
	fingerprint, err := trustCertificateFingerprint(caPEM)
	if err != nil {
		return fmt.Errorf("read ingress CA for cluster %q: %w", name, err)
	}
	if receipt, err := readTrustReceipt(name); err == nil {
		if receipt.Fingerprint == fingerprint {
			matched, validateErr := c.installedTrustEntryMatches(name, receipt, fingerprint)
			if validateErr != nil {
				return validateErr
			}
			if matched {
				if receipt.Pending {
					if err := finalizeTrustReceipt(receipt); err != nil {
						return trustReceiptFinalizeError(name, err)
					}
					_, err = fmt.Fprintf(c.out, "installed ingress CA trust for cluster %s (%s)\n", name, receipt.Store)
					return err
				}
				_, err = fmt.Fprintf(c.out, "ingress CA for cluster %s is already installed (%s)\n", name, receipt.Store)
				return err
			}
		} else {
			return fmt.Errorf("cluster %q has a different trusted CA recorded; run `tbx trust remove %s` before installing the current CA", name, name)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	var receipt *trustReceipt
	switch trustGOOS() {
	case "darwin":
		receipt, err = planDarwinTrust(name, fingerprint)
	case "linux":
		receipt, err = c.planLinuxTrust(name, caPath, fingerprint)
	default:
		err = unsupportedTrustPlatform(trustGOOS())
	}
	if err != nil {
		return err
	}
	if receipt == nil { // NixOS prints declarative instructions and mutates nothing.
		return nil
	}
	receipt.Pending = true
	if err := writeTrustReceipt(*receipt); err != nil {
		return err
	}
	switch receipt.Driver {
	case darwinTrustDriver:
		err = c.performDarwinTrust(*receipt, caPath)
	case linuxDebianDriver, linuxFedoraDriver, linuxP11KitDriver:
		err = performLinuxTrust(*receipt, caPEM)
	default:
		err = fmt.Errorf("unsupported trust driver %q", receipt.Driver)
	}
	if err != nil {
		return rollbackPendingTrustReceipt(name, err)
	}
	if err := finalizeTrustReceipt(*receipt); err != nil {
		return trustReceiptFinalizeError(name, err)
	}
	_, err = fmt.Fprintf(c.out, "installed ingress CA trust for cluster %s (%s)\n", name, receipt.Store)
	return err
}

func finalizeTrustReceipt(receipt trustReceipt) error {
	receipt.Pending = false
	receipt.InstalledAt = trustNow().UTC()
	return writeTrustReceipt(receipt)
}

func rollbackPendingTrustReceipt(name string, performErr error) error {
	path, err := trustReceiptPath(name)
	if err != nil {
		return fmt.Errorf("%w; also failed to locate the leftover pending trust receipt for rollback: %v", performErr, err)
	}
	if err := trustRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w; also failed to remove the leftover pending trust receipt %s: %v", performErr, path, err)
	}
	return performErr
}

func trustReceiptFinalizeError(name string, err error) error {
	return fmt.Errorf("trust store was updated but the receipt could not be finalized for cluster %s: %w; re-run `tbx trust install %s` to finalize it or `tbx trust remove %s` to undo it", name, err, name, name)
}

func (c cli) removeTrust(name string) error {
	if err := cluster.ValidateName(name); err != nil {
		return err
	}
	receipt, err := readTrustReceipt(name)
	if errors.Is(err, os.ErrNotExist) {
		_, err = fmt.Fprintf(c.out, "ingress CA trust for cluster %s is not installed; nothing to remove\n", name)
		return err
	}
	if err != nil {
		return err
	}

	switch receipt.Driver {
	case darwinTrustDriver:
		if trustGOOS() != "darwin" {
			return fmt.Errorf("trust receipt for cluster %q belongs to macOS; remove it on macOS", name)
		}
		if receipt.Pending {
			matched, matchErr := c.darwinTrustEntryMatches(name, receipt, receipt.Fingerprint)
			if matchErr != nil {
				return matchErr
			}
			if !matched {
				break
			}
		}
		err = c.removeDarwinTrust(receipt)
	case linuxDebianDriver, linuxFedoraDriver, linuxP11KitDriver:
		if trustGOOS() != "linux" {
			return fmt.Errorf("trust receipt for cluster %q belongs to Linux; remove it on Linux", name)
		}
		err = c.removeLinuxTrust(receipt)
	default:
		return fmt.Errorf("trust receipt for cluster %q names unsupported driver %q", name, receipt.Driver)
	}
	if err != nil {
		return err
	}
	path, err := trustReceiptPath(name)
	if err != nil {
		return err
	}
	if err := trustRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove trust receipt: %w", err)
	}
	_, err = fmt.Fprintf(c.out, "removed ingress CA trust for cluster %s\n", name)
	return err
}

func trustCertificateFingerprint(data []byte) (string, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return "", errors.New("PEM does not contain a certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse certificate: %w", err)
	}
	if !certificate.IsCA {
		return "", errors.New("certificate is not a CA")
	}
	digest := sha256.Sum256(certificate.Raw)
	return strings.ToUpper(hex.EncodeToString(digest[:])), nil
}

func trustReceiptPath(name string) (string, error) {
	home, err := trustUserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".talosbox", trustReceiptDirectory, name+".json"), nil
}

func readTrustReceipt(name string) (trustReceipt, error) {
	path, err := trustReceiptPath(name)
	if err != nil {
		return trustReceipt{}, err
	}
	data, err := trustReadFile(path)
	if err != nil {
		return trustReceipt{}, fmt.Errorf("read trust receipt: %w", err)
	}
	var receipt trustReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return trustReceipt{}, fmt.Errorf("decode trust receipt: %w", err)
	}
	if receipt.Cluster != name || receipt.Fingerprint == "" || receipt.Driver == "" || receipt.Path == "" {
		return trustReceipt{}, errors.New("trust receipt is incomplete or belongs to another cluster")
	}
	return receipt, nil
}

func writeTrustReceipt(receipt trustReceipt) error {
	path, err := trustReceiptPath(receipt.Cluster)
	if err != nil {
		return err
	}
	if err := trustMkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create trust receipt directory: %w", err)
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return fmt.Errorf("encode trust receipt: %w", err)
	}
	data = append(data, '\n')
	temporary := path + ".tmp"
	defer func() { _ = trustRemove(temporary) }()
	if err := trustWriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write trust receipt: %w", err)
	}
	if err := trustRename(temporary, path); err != nil {
		return fmt.Errorf("install trust receipt: %w", err)
	}
	return nil
}

func (c cli) installedTrustEntryMatches(clusterName string, receipt trustReceipt, fingerprint string) (bool, error) {
	switch receipt.Driver {
	case darwinTrustDriver:
		return c.darwinTrustEntryMatches(clusterName, receipt, fingerprint)
	case linuxDebianDriver, linuxFedoraDriver, linuxP11KitDriver:
		return linuxTrustEntryMatches(receipt, fingerprint)
	default:
		return false, fmt.Errorf("trust receipt for cluster %q names unsupported driver %q", clusterName, receipt.Driver)
	}
}

func runTrustCommandLive(command trustCommand, stdin io.Reader, stdout, stderr io.Writer) error {
	process := exec.Command(command.Name, command.Args...)
	process.Stdin = stdin
	process.Stdout = stdout
	process.Stderr = stderr
	return process.Run()
}

func reexecTrustWithSudo(action trustSystemAction, stdin []byte) error {
	executable, err := trustExecutable()
	if err != nil {
		return fmt.Errorf("find tbx executable: %w", err)
	}
	sudo, err := trustLookPath("sudo")
	if err != nil {
		return fmt.Errorf("find sudo: %w", err)
	}
	command := trustCommand{Name: sudo, Args: []string{
		executable, "trust", "_system", string(action.Operation), action.Driver, action.Fingerprint, action.Destination,
	}}
	if err := trustRunCommand(command, bytes.NewReader(stdin), os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("run sudo trust-store update: %w", err)
	}
	return nil
}

// Process, filesystem, identity and platform seams. Unit tests replace every
// host-sensitive decision, so they never touch a real keychain, sudo, or trust
// store.
var (
	trustGOOS          = nativeTrustGOOS
	trustUserHomeDir   = os.UserHomeDir
	trustIngressCAPath = provision.IngressCAPath
	trustReadFile      = os.ReadFile
	trustWriteFile     = os.WriteFile
	trustMkdirAll      = os.MkdirAll
	trustRemove        = os.Remove
	trustRename        = os.Rename
	trustStat          = os.Stat
	trustOpenFile      = os.OpenFile
	trustLookPath      = exec.LookPath
	trustEUID          = os.Geteuid
	trustExecutable    = os.Executable
	trustNow           = time.Now
	trustRunCommand    = runTrustCommandLive
	trustSudoReexec    = reexecTrustWithSudo
)
