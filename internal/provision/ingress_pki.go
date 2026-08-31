package provision

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	ingressTLSSecretName = "ingress-wildcard-tls"
	ingressLeafLifetime  = 397 * 24 * time.Hour
	ingressRenewBefore   = 30 * 24 * time.Hour
	ingressBackdateSkew  = 5 * time.Minute
)

type ingressPKIPaths struct {
	CACert  string
	CAKey   string
	TLSCert string
	TLSKey  string
}

type ingressPKIOptions struct {
	Now          func() time.Time
	Rand         io.Reader
	RenewDueLeaf bool
}

type ingressPKI struct {
	CACertPEM       []byte
	CAKeyPEM        []byte
	TLSCertPEM      []byte
	TLSKeyPEM       []byte
	CACertificate   *x509.Certificate
	LeafCertificate *x509.Certificate
	caKey           *ecdsa.PrivateKey
}

// IngressCAPath returns the per-cluster browser-trust root certificate path.
func IngressCAPath(name string) (string, error) {
	paths, err := ingressCertificatePaths(name)
	if err != nil {
		return "", err
	}
	return paths.CACert, nil
}

func ingressCertificatePaths(name string) (ingressPKIPaths, error) {
	dir, err := cluster.Dir(name)
	if err != nil {
		return ingressPKIPaths{}, err
	}
	return ingressPKIPathsForDir(dir), nil
}

func ingressPKIPathsForDir(dir string) ingressPKIPaths {
	return ingressPKIPaths{
		CACert:  filepath.Join(dir, "ingress-ca.crt"),
		CAKey:   filepath.Join(dir, "ingress-ca.key"),
		TLSCert: filepath.Join(dir, "ingress-tls.crt"),
		TLSKey:  filepath.Join(dir, "ingress-tls.key"),
	}
}

func loadIngressPKIForCluster(item cluster.Cluster) (ingressPKI, error) {
	paths, err := ingressCertificatePaths(item.Name)
	if err != nil {
		return ingressPKI{}, err
	}
	return loadIngressPKI(item, paths, ingressPKIOptions{})
}

func ensureIngressPKI(item cluster.Cluster, paths ingressPKIPaths, options ingressPKIOptions) (ingressPKI, error) {
	options.RenewDueLeaf = true
	return loadIngressPKI(item, paths, options)
}

func loadIngressPKI(item cluster.Cluster, paths ingressPKIPaths, options ingressPKIOptions) (ingressPKI, error) {
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	random := io.Reader(rand.Reader)
	if options.Rand != nil {
		random = options.Rand
	}

	existing, err := ingressPKIExisting(paths)
	if err != nil {
		return ingressPKI{}, err
	}
	if !existing.CACert && !existing.CAKey && !existing.TLSCert && !existing.TLSKey {
		return createIngressPKI(item, paths, now().UTC(), random)
	}
	pki, err := loadIngressCA(paths)
	if err != nil {
		return ingressPKI{}, ingressPKICorruptError(paths, err)
	}
	for _, path := range []string{paths.CACert, paths.CAKey} {
		if err := secureIngressPKIFile(path); err != nil {
			return ingressPKI{}, err
		}
	}
	wildcard := "*." + item.EffectiveDomain()
	leafErr := loadIngressLeaf(&pki, paths)
	if leafErr == nil {
		for _, path := range []string{paths.TLSCert, paths.TLSKey} {
			if err := secureIngressPKIFile(path); err != nil {
				return ingressPKI{}, err
			}
		}
	}
	if leafErr != nil || !ingressLeafMatchesWildcard(pki.LeafCertificate, wildcard) ||
		options.RenewDueLeaf && ingressLeafDueForRenewal(pki.LeafCertificate, now().UTC()) {
		if err := renewIngressLeaf(&pki, paths, wildcard, now().UTC(), random); err != nil {
			return ingressPKI{}, err
		}
	}
	return pki, nil
}

func createIngressPKI(item cluster.Cluster, paths ingressPKIPaths, now time.Time, random io.Reader) (ingressPKI, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return ingressPKI{}, fmt.Errorf("generate ingress CA key: %w", err)
	}
	serial, err := ingressSerial(random)
	if err != nil {
		return ingressPKI{}, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: item.Name + " talos-box ingress CA"},
		NotBefore:             now.Add(-ingressBackdateSkew),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(random, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return ingressPKI{}, fmt.Errorf("create ingress CA certificate: %w", err)
	}
	caCertificate, err := x509.ParseCertificate(der)
	if err != nil {
		return ingressPKI{}, fmt.Errorf("parse generated ingress CA certificate: %w", err)
	}
	caKeyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return ingressPKI{}, fmt.Errorf("encode ingress CA key: %w", err)
	}
	pki := ingressPKI{
		CACertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		CAKeyPEM:      pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER}),
		CACertificate: caCertificate,
		caKey:         caKey,
	}
	if err := writeSecurePair(paths.CACert, pki.CACertPEM, paths.CAKey, pki.CAKeyPEM); err != nil {
		return ingressPKI{}, fmt.Errorf("write ingress CA pair: %w", err)
	}
	if err := renewIngressLeaf(&pki, paths, "*."+item.EffectiveDomain(), now, random); err != nil {
		return ingressPKI{}, err
	}
	return pki, nil
}

func renewIngressLeaf(pki *ingressPKI, paths ingressPKIPaths, wildcard string, now time.Time, random io.Reader) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return fmt.Errorf("generate ingress TLS key: %w", err)
	}
	serial, err := ingressSerial(random)
	if err != nil {
		return err
	}
	notAfter := now.Add(ingressLeafLifetime)
	if notAfter.After(pki.CACertificate.NotAfter) {
		notAfter = pki.CACertificate.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: wildcard},
		DNSNames:     []string{wildcard},
		NotBefore:    now.Add(-ingressBackdateSkew),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(random, template, pki.CACertificate, &key.PublicKey, pki.caKey)
	if err != nil {
		return fmt.Errorf("create ingress TLS certificate: %w", err)
	}
	leafCertificate, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("parse generated ingress TLS certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode ingress TLS key: %w", err)
	}
	pki.TLSCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pki.TLSKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pki.LeafCertificate = leafCertificate
	if err := writeSecurePair(paths.TLSCert, pki.TLSCertPEM, paths.TLSKey, pki.TLSKeyPEM); err != nil {
		return fmt.Errorf("write ingress TLS pair: %w", err)
	}
	return nil
}

func decodePEMCertificate(data []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

func decodePEMKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "EC PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid EC private key PEM")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func publicKeysEqual(key *ecdsa.PublicKey, other any) bool {
	otherKey, ok := other.(*ecdsa.PublicKey)
	return ok && key.Equal(otherKey)
}

type ingressPKIExistingState struct {
	CACert  bool
	CAKey   bool
	TLSCert bool
	TLSKey  bool
}

func ingressPKIExisting(paths ingressPKIPaths) (ingressPKIExistingState, error) {
	check := func(path string) (bool, error) {
		if _, err := os.Stat(path); err == nil {
			return true, nil
		} else if errors.Is(err, os.ErrNotExist) {
			return false, nil
		} else {
			return false, fmt.Errorf("inspect ingress PKI %s: %w", filepath.Base(path), err)
		}
	}
	caCert, err := check(paths.CACert)
	if err != nil {
		return ingressPKIExistingState{}, err
	}
	caKey, err := check(paths.CAKey)
	if err != nil {
		return ingressPKIExistingState{}, err
	}
	tlsCert, err := check(paths.TLSCert)
	if err != nil {
		return ingressPKIExistingState{}, err
	}
	tlsKey, err := check(paths.TLSKey)
	if err != nil {
		return ingressPKIExistingState{}, err
	}
	return ingressPKIExistingState{CACert: caCert, CAKey: caKey, TLSCert: tlsCert, TLSKey: tlsKey}, nil
}

func loadIngressCA(paths ingressPKIPaths) (ingressPKI, error) {
	caCertPEM, err := readIngressPKIFile(paths.CACert)
	if err != nil {
		return ingressPKI{}, err
	}
	caKeyPEM, err := readIngressPKIFile(paths.CAKey)
	if err != nil {
		return ingressPKI{}, err
	}
	caCert, err := decodePEMCertificate(caCertPEM)
	if err != nil {
		return ingressPKI{}, fmt.Errorf("load ingress CA certificate: %w", err)
	}
	caKey, err := decodePEMKey(caKeyPEM)
	if err != nil {
		return ingressPKI{}, fmt.Errorf("load ingress CA key: %w", err)
	}
	if !caCert.IsCA || !publicKeysEqual(&caKey.PublicKey, caCert.PublicKey) {
		return ingressPKI{}, errors.New("ingress CA key does not match its certificate")
	}
	return ingressPKI{
		CACertPEM: caCertPEM, CAKeyPEM: caKeyPEM,
		CACertificate: caCert, caKey: caKey,
	}, nil
}

func loadIngressLeaf(pki *ingressPKI, paths ingressPKIPaths) error {
	tlsCertPEM, err := readIngressPKIFile(paths.TLSCert)
	if err != nil {
		return err
	}
	tlsKeyPEM, err := readIngressPKIFile(paths.TLSKey)
	if err != nil {
		return err
	}
	leaf, err := decodePEMCertificate(tlsCertPEM)
	if err != nil {
		return fmt.Errorf("load ingress TLS certificate: %w", err)
	}
	tlsKey, err := decodePEMKey(tlsKeyPEM)
	if err != nil {
		return fmt.Errorf("load ingress TLS key: %w", err)
	}
	if !publicKeysEqual(&tlsKey.PublicKey, leaf.PublicKey) {
		return errors.New("ingress TLS key does not match its certificate")
	}
	if err := leaf.CheckSignatureFrom(pki.CACertificate); err != nil {
		return fmt.Errorf("verify ingress TLS certificate: %w", err)
	}
	pki.TLSCertPEM = tlsCertPEM
	pki.TLSKeyPEM = tlsKeyPEM
	pki.LeafCertificate = leaf
	return nil
}

func readIngressPKIFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ingress PKI %s: %w", filepath.Base(path), err)
	}
	return data, nil
}

func ingressLeafMatchesWildcard(leaf *x509.Certificate, wildcard string) bool {
	return leaf != nil && len(leaf.DNSNames) == 1 && leaf.DNSNames[0] == wildcard
}

func ingressLeafDueForRenewal(leaf *x509.Certificate, now time.Time) bool {
	return leaf != nil && leaf.NotAfter.Sub(now) <= ingressRenewBefore
}

func secureIngressPKIFile(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure ingress PKI %s: %w", filepath.Base(path), err)
	}
	return nil
}

func ingressPKICorruptError(paths ingressPKIPaths, err error) error {
	return fmt.Errorf("ingress PKI in %s is incomplete or corrupt: %w; restore it or delete ingress-ca.crt, ingress-ca.key, ingress-tls.crt, and ingress-tls.key, then rerun `tbx up`",
		filepath.Dir(paths.CACert), err)
}

func writeSecurePair(firstPath string, firstData []byte, secondPath string, secondData []byte) (err error) {
	dir := filepath.Dir(firstPath)
	if filepath.Dir(secondPath) != dir {
		return fmt.Errorf("write secure pair: directories differ: %s, %s", firstPath, secondPath)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	firstTemp, err := writeSecureTemp(dir, firstPath, firstData)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(firstTemp) }()
	secondTemp, err := writeSecureTemp(dir, secondPath, secondData)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(secondTemp) }()
	firstBackup, firstHad, err := moveExistingSecureFile(firstPath)
	if err != nil {
		return err
	}
	secondBackup, secondHad, err := moveExistingSecureFile(secondPath)
	if err != nil {
		if firstHad {
			_ = os.Rename(firstBackup, firstPath)
		}
		return err
	}
	firstInstalled := false
	secondInstalled := false
	defer func() {
		if err == nil {
			_ = os.Remove(firstBackup)
			_ = os.Remove(secondBackup)
			return
		}
		if firstInstalled {
			_ = os.Remove(firstPath)
		}
		if secondInstalled {
			_ = os.Remove(secondPath)
		}
		if firstHad {
			_ = os.Rename(firstBackup, firstPath)
		}
		if secondHad {
			_ = os.Rename(secondBackup, secondPath)
		}
		_ = os.Remove(firstBackup)
		_ = os.Remove(secondBackup)
	}()
	if err = os.Rename(firstTemp, firstPath); err != nil {
		return err
	}
	firstInstalled = true
	firstTemp = ""
	if err = os.Rename(secondTemp, secondPath); err != nil {
		return err
	}
	secondInstalled = true
	secondTemp = ""
	return nil
}

func writeSecureTemp(dir, target string, data []byte) (string, error) {
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(target)+"-")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	return temporaryPath, nil
}

func moveExistingSecureFile(path string) (string, bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	placeholder, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-backup-")
	if err != nil {
		return "", false, err
	}
	backupPath := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		return "", false, err
	}
	if err := os.Remove(backupPath); err != nil {
		return "", false, err
	}
	if err := os.Rename(path, backupPath); err != nil {
		return "", false, err
	}
	return backupPath, true, nil
}

func ingressSerial(random io.Reader) (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(random, limit)
	if err != nil {
		return nil, fmt.Errorf("generate ingress certificate serial: %w", err)
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial, nil
}

func ingressTLSSecretObject(pki ingressPKI) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name": ingressTLSSecretName, "namespace": probeNamespace,
			"labels": map[string]any{"talosbox.dev/managed": "true"},
		},
		"type": "kubernetes.io/tls",
		"data": map[string]any{
			"tls.crt": encodeSecretData(pki.TLSCertPEM),
			"tls.key": encodeSecretData(pki.TLSKeyPEM),
		},
	}}
}

func inspectionIngressTLSSecretObject() unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name": ingressTLSSecretName, "namespace": probeNamespace,
			"labels":      map[string]any{"talosbox.dev/managed": "true"},
			"annotations": map[string]any{"talosbox.dev/inspection": "TLS data is generated in the cluster credential directory and redacted here"},
		},
		"type": "kubernetes.io/tls",
	}}
}

func encodeSecretData(data []byte) string { return base64.StdEncoding.EncodeToString(data) }
