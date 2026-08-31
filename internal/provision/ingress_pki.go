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
)

type ingressPKIPaths struct {
	CACert  string
	CAKey   string
	TLSCert string
	TLSKey  string
}

type ingressPKIOptions struct {
	Now  func() time.Time
	Rand io.Reader
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
	return loadIngressPKI(paths)
}

func ensureIngressPKI(item cluster.Cluster, paths ingressPKIPaths, options ingressPKIOptions) (ingressPKI, error) {
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	random := io.Reader(rand.Reader)
	if options.Rand != nil {
		random = options.Rand
	}

	files := []string{paths.CACert, paths.CAKey, paths.TLSCert, paths.TLSKey}
	existing := 0
	for _, path := range files {
		if _, err := os.Stat(path); err == nil {
			existing++
		} else if !errors.Is(err, os.ErrNotExist) {
			return ingressPKI{}, fmt.Errorf("inspect ingress PKI %s: %w", filepath.Base(path), err)
		}
	}
	if existing == 0 {
		return createIngressPKI(item, paths, now().UTC(), random)
	}
	if existing != len(files) {
		return ingressPKI{}, errors.New("refuse to replace incomplete ingress PKI")
	}

	pki, err := loadIngressPKI(paths)
	if err != nil {
		return ingressPKI{}, err
	}
	for _, path := range files {
		if err := os.Chmod(path, 0o600); err != nil {
			return ingressPKI{}, fmt.Errorf("secure ingress PKI %s: %w", filepath.Base(path), err)
		}
	}
	wildcard := "*." + item.EffectiveDomain()
	if pki.LeafCertificate.NotAfter.Sub(now().UTC()) <= ingressRenewBefore ||
		len(pki.LeafCertificate.DNSNames) != 1 || pki.LeafCertificate.DNSNames[0] != wildcard {
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
		NotBefore:             now,
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
	if err := writeSecure(paths.CACert, pki.CACertPEM); err != nil {
		return ingressPKI{}, fmt.Errorf("write ingress CA certificate: %w", err)
	}
	if err := writeSecure(paths.CAKey, pki.CAKeyPEM); err != nil {
		return ingressPKI{}, fmt.Errorf("write ingress CA key: %w", err)
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
		NotBefore:    now,
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
	if err := writeSecure(paths.TLSCert, pki.TLSCertPEM); err != nil {
		return fmt.Errorf("write ingress TLS certificate: %w", err)
	}
	if err := writeSecure(paths.TLSKey, pki.TLSKeyPEM); err != nil {
		return fmt.Errorf("write ingress TLS key: %w", err)
	}
	return nil
}

func loadIngressPKI(paths ingressPKIPaths) (ingressPKI, error) {
	read := func(path string) ([]byte, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read ingress PKI %s: %w", filepath.Base(path), err)
		}
		return data, nil
	}
	caCertPEM, err := read(paths.CACert)
	if err != nil {
		return ingressPKI{}, err
	}
	caKeyPEM, err := read(paths.CAKey)
	if err != nil {
		return ingressPKI{}, err
	}
	tlsCertPEM, err := read(paths.TLSCert)
	if err != nil {
		return ingressPKI{}, err
	}
	tlsKeyPEM, err := read(paths.TLSKey)
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
	leaf, err := decodePEMCertificate(tlsCertPEM)
	if err != nil {
		return ingressPKI{}, fmt.Errorf("load ingress TLS certificate: %w", err)
	}
	tlsKey, err := decodePEMKey(tlsKeyPEM)
	if err != nil {
		return ingressPKI{}, fmt.Errorf("load ingress TLS key: %w", err)
	}
	if !caCert.IsCA || !publicKeysEqual(&caKey.PublicKey, caCert.PublicKey) {
		return ingressPKI{}, errors.New("ingress CA key does not match its certificate")
	}
	if !publicKeysEqual(&tlsKey.PublicKey, leaf.PublicKey) {
		return ingressPKI{}, errors.New("ingress TLS key does not match its certificate")
	}
	if err := leaf.CheckSignatureFrom(caCert); err != nil {
		return ingressPKI{}, fmt.Errorf("verify ingress TLS certificate: %w", err)
	}
	return ingressPKI{
		CACertPEM: caCertPEM, CAKeyPEM: caKeyPEM, TLSCertPEM: tlsCertPEM, TLSKeyPEM: tlsKeyPEM,
		CACertificate: caCert, LeafCertificate: leaf, caKey: caKey,
	}, nil
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
