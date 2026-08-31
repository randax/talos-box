package provision

import (
	"bytes"
	"crypto/x509"
	"errors"
	"io"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestEnsureIngressPKICreatesAndReusesWildcardCertificates(t *testing.T) {
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	paths := testIngressPKIPaths(t)
	item := cluster.Cluster{Name: "demo", Domain: "demo.lab.internal"}

	first, err := ensureIngressPKI(item, paths, ingressPKIOptions{
		Now:  fixedTime(now),
		Rand: newDeterministicReader(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.CACertificate.IsCA {
		t.Fatal("CA certificate is not a CA")
	}
	if got, want := first.CACertificate.NotBefore, now.Add(-ingressBackdateSkew); !got.Equal(want) {
		t.Fatalf("CA NotBefore = %s, want %s", got, want)
	}
	if got, want := first.CACertificate.NotAfter.Sub(first.CACertificate.NotBefore), first.CACertificate.NotBefore.AddDate(10, 0, 0).Sub(first.CACertificate.NotBefore); got != want {
		t.Fatalf("CA lifetime = %s, want %s", got, want)
	}
	if got := first.LeafCertificate.DNSNames; len(got) != 1 || got[0] != "*.demo.lab.internal" {
		t.Fatalf("leaf SANs = %v, want only *.demo.lab.internal", got)
	}
	if got, want := first.LeafCertificate.NotBefore, now.Add(-ingressBackdateSkew); !got.Equal(want) {
		t.Fatalf("leaf NotBefore = %s, want %s", got, want)
	}
	if got := first.LeafCertificate.NotAfter.Sub(first.LeafCertificate.NotBefore); got != ingressLeafLifetime {
		t.Fatalf("leaf lifetime = %s, want %s", got, ingressLeafLifetime)
	}
	for _, path := range []string{paths.CACert, paths.CAKey, paths.TLSCert, paths.TLSKey} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %#o, want 0600", filepath.Base(path), got)
		}
	}

	second, err := ensureIngressPKI(item, paths, ingressPKIOptions{
		Now:  fixedTime(now.Add(24 * time.Hour)),
		Rand: newDeterministicReader(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2][]byte{
		"ca.crt":  {first.CACertPEM, second.CACertPEM},
		"ca.key":  {first.CAKeyPEM, second.CAKeyPEM},
		"tls.crt": {first.TLSCertPEM, second.TLSCertPEM},
		"tls.key": {first.TLSKeyPEM, second.TLSKeyPEM},
	} {
		if !bytes.Equal(pair[0], pair[1]) {
			t.Fatalf("%s changed across reuse", name)
		}
	}
}

func TestEnsureIngressPKIRenewsOnlyTheLeafNearExpiry(t *testing.T) {
	createdAt := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	item := cluster.Cluster{Name: "demo"}
	paths := testIngressPKIPaths(t)

	first, err := ensureIngressPKI(item, paths, ingressPKIOptions{
		Now:  fixedTime(createdAt),
		Rand: newDeterministicReader(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureIngressPKI(item, paths, ingressPKIOptions{
		Now:  fixedTime(first.LeafCertificate.NotAfter.Add(-29 * 24 * time.Hour)),
		Rand: newDeterministicReader(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.CACertPEM, second.CACertPEM) || !bytes.Equal(first.CAKeyPEM, second.CAKeyPEM) {
		t.Fatal("CA changed during leaf renewal")
	}
	if bytes.Equal(first.TLSCertPEM, second.TLSCertPEM) || bytes.Equal(first.TLSKeyPEM, second.TLSKeyPEM) {
		t.Fatal("leaf was not renewed")
	}
	if got := second.LeafCertificate.DNSNames; len(got) != 1 || got[0] != "*.demo.k8s.test" {
		t.Fatalf("renewed leaf SANs = %v", got)
	}
	if got := second.LeafCertificate.NotAfter.Sub(second.LeafCertificate.NotBefore); got != ingressLeafLifetime {
		t.Fatalf("renewed leaf lifetime = %s, want %s", got, ingressLeafLifetime)
	}
}

func TestEnsureIngressPKIRepairsInterruptedLeafPublication(t *testing.T) {
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	paths := testIngressPKIPaths(t)
	item := cluster.Cluster{Name: "demo"}
	first, err := ensureIngressPKI(item, paths, ingressPKIOptions{
		Now:  fixedTime(now),
		Rand: newDeterministicReader(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths.TLSKey); err != nil {
		t.Fatal(err)
	}
	second, err := ensureIngressPKI(item, paths, ingressPKIOptions{
		Now:  fixedTime(now.Add(48 * time.Hour)),
		Rand: newDeterministicReader(6),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.CACertPEM, second.CACertPEM) || !bytes.Equal(first.CAKeyPEM, second.CAKeyPEM) {
		t.Fatal("CA changed while repairing interrupted leaf publication")
	}
	if bytes.Equal(first.TLSCertPEM, second.TLSCertPEM) || bytes.Equal(first.TLSKeyPEM, second.TLSKeyPEM) {
		t.Fatal("leaf pair was not regenerated after interrupted publication")
	}
}

func TestEnsureIngressPKIRepairsBrokenLeafFiles(t *testing.T) {
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	item := cluster.Cluster{Name: "demo"}

	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, paths ingressPKIPaths, item cluster.Cluster)
	}{
		{
			name: "corrupt certificate PEM",
			mutate: func(t *testing.T, paths ingressPKIPaths, _ cluster.Cluster) {
				t.Helper()
				if err := os.WriteFile(paths.TLSCert, []byte("not a certificate"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unreadable certificate path",
			mutate: func(t *testing.T, paths ingressPKIPaths, _ cluster.Cluster) {
				t.Helper()
				if err := os.Remove(paths.TLSCert); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(paths.TLSCert, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "mismatched key",
			mutate: func(t *testing.T, paths ingressPKIPaths, item cluster.Cluster) {
				t.Helper()
				other, err := ensureIngressPKI(item, testIngressPKIPaths(t), ingressPKIOptions{
					Now:  fixedTime(now.Add(time.Hour)),
					Rand: newDeterministicReader(7),
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.TLSKey, other.TLSKeyPEM, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "leaf signed by another CA",
			mutate: func(t *testing.T, paths ingressPKIPaths, item cluster.Cluster) {
				t.Helper()
				other, err := ensureIngressPKI(item, testIngressPKIPaths(t), ingressPKIOptions{
					Now:  fixedTime(now.Add(2 * time.Hour)),
					Rand: newDeterministicReader(8),
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.TLSCert, other.TLSCertPEM, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.TLSKey, other.TLSKeyPEM, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := testIngressPKIPaths(t)
			first, err := ensureIngressPKI(item, paths, ingressPKIOptions{
				Now:  fixedTime(now),
				Rand: newDeterministicReader(9),
			})
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, paths, item)
			second, err := ensureIngressPKI(item, paths, ingressPKIOptions{
				Now:  fixedTime(now.Add(48 * time.Hour)),
				Rand: newDeterministicReader(10),
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.CACertPEM, second.CACertPEM) || !bytes.Equal(first.CAKeyPEM, second.CAKeyPEM) {
				t.Fatal("CA changed while repairing a broken leaf")
			}
			if bytes.Equal(first.TLSCertPEM, second.TLSCertPEM) || bytes.Equal(first.TLSKeyPEM, second.TLSKeyPEM) {
				t.Fatal("leaf pair was not regenerated")
			}
		})
	}
}

func TestEnsureIngressPKIRecoversIncompleteCorruptCAWithoutLeafPair(t *testing.T) {
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	item := cluster.Cluster{Name: "demo"}

	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, paths ingressPKIPaths)
	}{
		{
			name: "missing CA key",
			mutate: func(t *testing.T, paths ingressPKIPaths) {
				t.Helper()
				if err := os.Remove(paths.CAKey); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt CA certificate",
			mutate: func(t *testing.T, paths ingressPKIPaths) {
				t.Helper()
				if err := os.WriteFile(paths.CACert, []byte("not a certificate"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := testIngressPKIPaths(t)
			first, err := ensureIngressPKI(item, paths, ingressPKIOptions{
				Now:  fixedTime(now),
				Rand: newDeterministicReader(11),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(paths.TLSCert); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(paths.TLSKey); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, paths)
			second, err := ensureIngressPKI(item, paths, ingressPKIOptions{
				Now:  fixedTime(now.Add(48 * time.Hour)),
				Rand: newDeterministicReader(12),
			})
			if err != nil {
				t.Fatal(err)
			}
			assertIngressPKIHealthy(t, item, paths, second)
			if bytes.Equal(first.CACertPEM, second.CACertPEM) && bytes.Equal(first.CAKeyPEM, second.CAKeyPEM) {
				t.Fatal("interrupted first generation reused the incomplete CA")
			}
		})
	}
}

func TestEnsureIngressPKIRefusesCorruptCAWithRecoveryGuidance(t *testing.T) {
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	item := cluster.Cluster{Name: "demo"}

	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, paths ingressPKIPaths)
	}{
		{
			name: "missing CA key",
			mutate: func(t *testing.T, paths ingressPKIPaths) {
				t.Helper()
				if err := os.Remove(paths.CAKey); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt CA certificate",
			mutate: func(t *testing.T, paths ingressPKIPaths) {
				t.Helper()
				if err := os.WriteFile(paths.CACert, []byte("not a certificate"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := testIngressPKIPaths(t)
			if _, err := ensureIngressPKI(item, paths, ingressPKIOptions{
				Now:  fixedTime(now),
				Rand: newDeterministicReader(13),
			}); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, paths)
			_, err := ensureIngressPKI(item, paths, ingressPKIOptions{
				Now:  fixedTime(now.Add(48 * time.Hour)),
				Rand: newDeterministicReader(14),
			})
			if err == nil {
				t.Fatal("corrupt ingress CA was accepted")
			}
			if !strings.Contains(err.Error(), filepath.Dir(paths.CACert)) {
				t.Fatalf("error %q did not name the cluster directory", err)
			}
			if !strings.Contains(err.Error(), "restore it or delete ingress-ca.crt, ingress-ca.key, ingress-tls.crt, and ingress-tls.key") {
				t.Fatalf("error %q did not include recovery guidance", err)
			}
			if !strings.Contains(err.Error(), "rerun `tbx up`") {
				t.Fatalf("error %q did not tell the user to rerun tbx up", err)
			}
		})
	}
}

func TestEnsureIngressPKIRecoversFromPublicationCrashes(t *testing.T) {
	if crashAfter := os.Getenv("TBX_INGRESS_PKI_CRASH_AFTER"); crashAfter != "" {
		dir := os.Getenv("TBX_INGRESS_PKI_DIR")
		if dir == "" {
			t.Fatal("TBX_INGRESS_PKI_DIR is empty")
		}
		ingressPKIPublishHook = func(path string) {
			if filepath.Base(path) == crashAfter {
				os.Exit(86)
			}
		}
		defer func() { ingressPKIPublishHook = nil }()
		_, _ = ensureIngressPKI(cluster.Cluster{Name: "demo"}, ingressPKIPathsForDir(dir), ingressPKIOptions{
			Now:  fixedTime(time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)),
			Rand: newDeterministicReader(14),
		})
		t.Fatalf("expected simulated crash after publishing %s", crashAfter)
	}

	item := cluster.Cluster{Name: "demo"}
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	for _, crashAfter := range []string{"ingress-ca.crt", "ingress-ca.key", "ingress-tls.crt", "ingress-tls.key"} {
		t.Run(crashAfter, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run", "^TestEnsureIngressPKIRecoversFromPublicationCrashes$")
			cmd.Env = append(os.Environ(),
				"TBX_INGRESS_PKI_CRASH_AFTER="+crashAfter,
				"TBX_INGRESS_PKI_DIR="+dir,
			)
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("crash runner error = %v, want exit status 86", err)
			}

			paths := ingressPKIPathsForDir(dir)
			pki, err := ensureIngressPKI(item, paths, ingressPKIOptions{
				Now:  fixedTime(now.Add(2 * time.Hour)),
				Rand: newDeterministicReader(15),
			})
			if err != nil {
				t.Fatal(err)
			}
			assertIngressPKIHealthy(t, item, paths, pki)
		})
	}
}

func TestIngressTLSSecretObjectCarriesExactPEMBytes(t *testing.T) {
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	item := cluster.Cluster{Name: "demo"}
	paths := testIngressPKIPaths(t)
	pki, err := ensureIngressPKI(item, paths, ingressPKIOptions{
		Now:  fixedTime(now),
		Rand: newDeterministicReader(7),
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := ingressTLSSecretObject(pki)
	if secret.GetNamespace() != probeNamespace || secret.GetName() != ingressTLSSecretName {
		t.Fatalf("Secret identity = %s/%s", secret.GetNamespace(), secret.GetName())
	}
	if got := secret.GetLabels()["talosbox.dev/managed"]; got != "true" {
		t.Fatalf("Secret managed label = %q", got)
	}
	if got, _, _ := unstructured.NestedString(secret.Object, "type"); got != "kubernetes.io/tls" {
		t.Fatalf("Secret type = %q", got)
	}
	if got, _, _ := unstructured.NestedString(secret.Object, "data", "tls.crt"); got != encodeSecretData(pki.TLSCertPEM) {
		t.Fatal("tls.crt payload did not match the generated PEM")
	}
	if got, _, _ := unstructured.NestedString(secret.Object, "data", "tls.key"); got != encodeSecretData(pki.TLSKeyPEM) {
		t.Fatal("tls.key payload did not match the generated PEM")
	}
}

func assertIngressPKIHealthy(t *testing.T, item cluster.Cluster, paths ingressPKIPaths, pki ingressPKI) {
	t.Helper()
	if !pki.CACertificate.IsCA {
		t.Fatal("CA certificate is not a CA")
	}
	if err := pki.LeafCertificate.VerifyHostname("probe." + item.EffectiveDomain()); err != nil {
		t.Fatalf("VerifyHostname() error = %v", err)
	}
	reloaded, err := loadIngressPKI(item, paths, ingressPKIOptions{})
	if err != nil {
		t.Fatalf("reload ingress PKI: %v", err)
	}
	if !bytes.Equal(reloaded.CACertPEM, pki.CACertPEM) || !bytes.Equal(reloaded.CAKeyPEM, pki.CAKeyPEM) ||
		!bytes.Equal(reloaded.TLSCertPEM, pki.TLSCertPEM) || !bytes.Equal(reloaded.TLSKeyPEM, pki.TLSKeyPEM) {
		t.Fatal("reloaded ingress PKI differs from the generated files")
	}
}

func testIngressPKIPaths(t *testing.T) ingressPKIPaths {
	t.Helper()
	dir := t.TempDir()
	return ingressPKIPaths{
		CACert:  filepath.Join(dir, "ingress-ca.crt"),
		CAKey:   filepath.Join(dir, "ingress-ca.key"),
		TLSCert: filepath.Join(dir, "ingress-tls.crt"),
		TLSKey:  filepath.Join(dir, "ingress-tls.key"),
	}
}

func fixedTime(now time.Time) func() time.Time {
	return func() time.Time { return now }
}

func newDeterministicReader(seed int64) io.Reader {
	source := mathrand.New(mathrand.NewSource(seed))
	return deterministicReader{source: source}
}

type deterministicReader struct{ source *mathrand.Rand }

func (reader deterministicReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(reader.source.Intn(255) + 1)
	}
	return len(p), nil
}

func TestDecodePEMCertificateRejectsNonCertificateData(t *testing.T) {
	if _, err := decodePEMCertificate([]byte("nope")); err == nil {
		t.Fatal("decodePEMCertificate accepted invalid data")
	}
}

func TestDecodePEMKeyRejectsNonKeyData(t *testing.T) {
	if _, err := decodePEMKey([]byte("nope")); err == nil {
		t.Fatal("decodePEMKey accepted invalid data")
	}
}

func TestDecodePEMCertificateParsesTheGeneratedLeaf(t *testing.T) {
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	paths := testIngressPKIPaths(t)
	pki, err := ensureIngressPKI(cluster.Cluster{Name: "demo"}, paths, ingressPKIOptions{
		Now:  fixedTime(now),
		Rand: newDeterministicReader(8),
	})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := decodePEMCertificate(pki.TLSCertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("probe.demo.k8s.test"); err != nil {
		t.Fatalf("VerifyHostname() error = %v", err)
	}
	if _, err := x509.ParseCertificate(leaf.Raw); err != nil {
		t.Fatalf("leaf reparse error = %v", err)
	}
}
