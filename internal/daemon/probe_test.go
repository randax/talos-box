package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// The production probe is fixed to :50000, so classification is tested via
// probeHostPort against local listeners presenting controlled certificates.
func TestProbeClassifiesListeners(t *testing.T) {
	t.Run("plaintext listener reads as dialed but not TLS", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 16)
				_, _ = c.Read(buf)
				_ = c.Close() // plaintext server slams a TLS ClientHello
			}
		}()
		probe := probeHostPort(ln.Addr().String())
		if !probe.Dialed || probe.TLS {
			t.Errorf("probe = %+v, want Dialed && !TLS", probe)
		}
	})

	t.Run("maintenance certificate is recognized", func(t *testing.T) {
		cert := selfSigned(t, "maintenance-service.talos.dev")
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		go acceptTLS(ln)
		probe := probeHostPort(ln.Addr().String())
		if !probe.Dialed || !probe.TLS || !probe.MaintenanceCert {
			t.Errorf("probe = %+v, want Dialed && TLS && MaintenanceCert", probe)
		}
	})

	t.Run("cluster certificate reads as configured", func(t *testing.T) {
		cert := selfSigned(t, "talos-abc-123")
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		go acceptTLS(ln)
		probe := probeHostPort(ln.Addr().String())
		if !probe.Dialed || !probe.TLS || probe.MaintenanceCert {
			t.Errorf("probe = %+v, want Dialed && TLS && !MaintenanceCert", probe)
		}
	})

	t.Run("closed port reads as not dialed", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		_ = ln.Close()
		probe := probeHostPort(addr)
		if probe.Dialed {
			t.Errorf("probe = %+v, want !Dialed", probe)
		}
	})
}

func acceptTLS(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.(*tls.Conn).Handshake()
		_ = c.Close()
	}
}

func selfSigned(t *testing.T, commonName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
