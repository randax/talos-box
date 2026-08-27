package main

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestParseAllowedUID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want *uint32
	}{
		{name: "unset"},
		{name: "configured", args: []string{"--allowed-uid", "501"}, want: uint32Pointer(501)},
		{name: "root", args: []string{"--allowed-uid=0"}, want: uint32Pointer(0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseAllowedUID(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || test.want == nil {
				if got != nil || test.want != nil {
					t.Fatalf("allowed uid = %v, want %v", got, test.want)
				}
				return
			}
			if *got != *test.want {
				t.Fatalf("allowed uid = %d, want %d", *got, *test.want)
			}
		})
	}
}

func TestParseAllowedUIDRejectsInvalidValue(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "-1", "not-a-uid", "4294967296"} {
		if _, err := parseAllowedUID([]string{"--allowed-uid", value}); err == nil {
			t.Fatalf("parseAllowedUID accepted %q", value)
		}
	}
}

func TestServerAllowedUIDUsesSocketAdmissionOnlyWhenImplicitAndActivated(t *testing.T) {
	t.Parallel()

	serviceUID := uint32(995)
	userUID := uint32(501)
	if got := serverAllowedUID(&serviceUID, false, true); got != nil {
		t.Fatalf("implicit activated UID = %v, want socket admission", got)
	}
	if got := serverAllowedUID(&userUID, true, true); got == nil || *got != userUID {
		t.Fatalf("explicit activated UID = %v, want %d", got, userUID)
	}
	if got := serverAllowedUID(&userUID, false, false); got == nil || *got != userUID {
		t.Fatalf("manual helper UID = %v, want %d", got, userUID)
	}
}

func uint32Pointer(value uint32) *uint32 {
	return &value
}

func TestOpenHelperListenerSkipsPathResolutionWhenActivated(t *testing.T) {
	t.Parallel()

	inheritedListener := fakeListener{}
	resolved := false
	listener, path, activated, err := openHelperListener(
		func(string) (net.Listener, bool, error) { return inheritedListener, true, nil },
		func() (string, error) {
			resolved = true
			return "", errors.New("resolve must not run under socket activation")
		},
		func(string) (net.Listener, error) { return nil, errors.New("listen must not run under socket activation") },
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved {
		t.Fatal("openHelperListener resolved the socket path under socket activation")
	}
	if !activated {
		t.Fatal("openHelperListener reported activated = false")
	}
	if path != "" {
		t.Fatalf("socket path = %q, want empty under socket activation", path)
	}
	if listener != inheritedListener {
		t.Fatalf("listener = %v, want the inherited listener", listener)
	}
}

func TestOpenHelperListenerBindsResolvedPathWhenNotActivated(t *testing.T) {
	t.Parallel()

	bound := fakeListener{}
	var listenPath string
	listener, path, activated, err := openHelperListener(
		func(string) (net.Listener, bool, error) { return nil, false, nil },
		func() (string, error) { return "/run/tbx-helper.sock", nil },
		func(candidate string) (net.Listener, error) {
			listenPath = candidate
			return bound, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated {
		t.Fatal("openHelperListener reported activated = true without an inherited FD")
	}
	if listenPath != "/run/tbx-helper.sock" || path != "/run/tbx-helper.sock" {
		t.Fatalf("listen path = %q, returned path = %q", listenPath, path)
	}
	if listener != bound {
		t.Fatalf("listener = %v, want the bound listener", listener)
	}
}

func TestOpenHelperListenerReportsResolveFailureWhenNotActivated(t *testing.T) {
	t.Parallel()

	_, _, _, err := openHelperListener(
		func(string) (net.Listener, bool, error) { return nil, false, nil },
		func() (string, error) { return "", errors.New("boom") },
		func(string) (net.Listener, error) { return nil, errors.New("listen must not run") },
	)
	if err == nil || !strings.Contains(err.Error(), "resolve helper socket path") {
		t.Fatalf("error = %v, want a resolve helper socket path failure", err)
	}
}

type fakeListener struct{ net.Listener }
