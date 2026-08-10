package helper

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestResolvedRegistrationUsesPerClusterRouteOnlyDomain(t *testing.T) {
	t.Parallel()

	var calls [][]string
	registration := applyResolvedRegistration("demo", 7, func(name string, args ...string) error {
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

func TestResolvedRegistrationFailureIsNonFatalAndActionable(t *testing.T) {
	t.Parallel()

	registration := applyResolvedRegistration("demo", 7, func(string, ...string) error {
		return errors.New("resolvectl not found")
	})
	if registration.Registered {
		t.Fatalf("registration = %#v, want degraded", registration)
	}
	if registration.Detail == "" || registration.ManualStep == "" {
		t.Fatalf("registration lacks detail or manual step: %#v", registration)
	}
}

func TestDispatchDNSListenReturnsOwnedDescriptorAndRegistration(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "dns-listener")
	if err != nil {
		t.Fatal(err)
	}

	originalBind := bindDNS
	originalRegister := registerDNS
	bindDNS = func(subnetIndex int) (*os.File, error) {
		if subnetIndex != 7 {
			t.Fatalf("bind subnet = %d, want 7", subnetIndex)
		}
		return file, nil
	}
	registerDNS = func(cluster string, subnetIndex int) DNSRegistration {
		if cluster != "demo" || subnetIndex != 7 {
			t.Fatalf("register = %s/%d, want demo/7", cluster, subnetIndex)
		}
		return DNSRegistration{Registered: true, Domain: "~demo.k8s.test"}
	}
	t.Cleanup(func() {
		bindDNS = originalBind
		registerDNS = originalRegister
		_ = file.Close()
	})

	reply := NewServer(nil).dispatch(Request{
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
