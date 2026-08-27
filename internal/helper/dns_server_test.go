package helper

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestResolvedRegistrationUsesPerClusterRouteOnlyDomain(t *testing.T) {
	t.Parallel()

	var calls [][]string
	registration := applyResolvedRegistration("demo.k8s.test", 7, func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	})
	wantCalls := [][]string{
		{"resolvectl", "dns", "br-tbx7", "172.30.7.1"},
		{"resolvectl", "domain", "br-tbx7", "~demo.k8s.test"},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("resolved calls = %#v, want %#v", calls, wantCalls)
	}
	if !registration.Registered || registration.Domain != "~demo.k8s.test" {
		t.Fatalf("registration = %#v", registration)
	}
}

func TestResolvedRegistrationUsesCustomDomain(t *testing.T) {
	t.Parallel()

	var domains []string
	registration := applyResolvedRegistration("staging.lab.internal", 9, func(name string, args ...string) error {
		if len(args) > 0 && args[0] == "domain" {
			domains = append(domains, args[2])
		}
		return nil
	})
	if !reflect.DeepEqual(domains, []string{"~staging.lab.internal"}) {
		t.Fatalf("registered domains = %v, want [~staging.lab.internal]", domains)
	}
	if !registration.Registered || registration.Domain != "~staging.lab.internal" {
		t.Fatalf("registration = %#v", registration)
	}
}

func TestResolvedRegistrationFailureIsNonFatalAndActionable(t *testing.T) {
	t.Parallel()

	registration := applyResolvedRegistration("demo.k8s.test", 7, func(string, ...string) error {
		return errors.New("resolvectl not found")
	})
	if registration.Registered {
		t.Fatalf("registration = %#v, want degraded", registration)
	}
	if registration.Detail == "" || registration.ManualStep == "" {
		t.Fatalf("registration lacks detail or manual step: %#v", registration)
	}
}

func TestDecodeDNSIdentityRefusesHostileDomains(t *testing.T) {
	t.Parallel()

	// The helper is root and hands the domain to resolvectl; anything but a
	// canonical validated domain must be refused (a bare "." would register
	// "~." and hijack all host DNS).
	for _, raw := range []string{
		`{"cluster":"a","domain":".","subnetIndex":0}`,
		`{"cluster":"a","domain":"","subnetIndex":0}`, // empty falls back to the default — allowed
		`{"cluster":"a","domain":"Lab.Internal","subnetIndex":0}`,
		`{"cluster":"a","domain":"lab.internal.","subnetIndex":0}`,
		`{"cluster":"a","domain":"../etc","subnetIndex":0}`,
		`{"cluster":"a","domain":"a/b.test","subnetIndex":0}`,
	} {
		domain, _, err := decodeDNSIdentity([]byte(raw))
		if strings.Contains(raw, `"domain":""`) {
			if err != nil || domain != "a.k8s.test" {
				t.Fatalf("empty domain: got %q, %v; want default a.k8s.test", domain, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("decodeDNSIdentity accepted %s as %q", raw, domain)
		}
	}
}

func TestDispatchDNSListenReturnsOwnedDescriptorAndRegistration(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "dns-listener")
	if err != nil {
		t.Fatal(err)
	}

	originalBind := bindDNS
	originalRegister := registerDNS
	bindDNS = func(_ []int, subnetIndex int) (*os.File, error) {
		if subnetIndex != 7 {
			t.Fatalf("bind subnet = %d, want 7", subnetIndex)
		}
		return file, nil
	}
	registerDNS = func(domain string, subnetIndex int) DNSRegistration {
		if domain != "demo.k8s.test" || subnetIndex != 7 {
			t.Fatalf("register = %s/%d, want demo.k8s.test/7", domain, subnetIndex)
		}
		return DNSRegistration{Registered: true, Domain: "~demo.k8s.test"}
	}
	t.Cleanup(func() {
		bindDNS = originalBind
		registerDNS = originalRegister
		_ = file.Close()
	})

	reply := NewServer(nil, nil).dispatch(Request{
		Op:   "dns.listen",
		Args: json.RawMessage(`{"cluster":"demo","subnetIndex":7}`),
	})
	if !reply.response.OK || reply.fd != int(file.Fd()) || reply.finalize == nil {
		t.Fatalf("reply = %#v", reply)
	}
	reply.finalize()
	if _, err := file.Stat(); err == nil {
		t.Fatal("passed descriptor remained open after response finalization")
	}
}
