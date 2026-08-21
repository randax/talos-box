package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// TestMirrorOfflineFinding pins the doctor half of #403: offline mode silently
// changes how every pull on the host fails, so doctor names it.
func TestMirrorOfflineFinding(t *testing.T) {
	tests := []struct {
		name      string
		offline   func() (bool, error)
		wantLevel string
		wantParts []string
	}{
		{
			name:      "on warns prominently",
			offline:   func() (bool, error) { return true, nil },
			wantLevel: "WARN",
			wantParts: []string{"mirror offline is on", "tbx mirror offline off"},
		},
		{
			name:      "off passes",
			offline:   func() (bool, error) { return false, nil },
			wantLevel: "PASS",
			wantParts: []string{"off"},
		},
		{
			name:      "daemon down skips",
			offline:   func() (bool, error) { return false, dialError{err: errors.New("connection refused")} },
			wantLevel: "SKIP",
		},
		{
			name:      "no probe skips",
			wantLevel: "SKIP",
			wantParts: []string{"probe unavailable"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			finding := mirrorOfflineFinding(test.offline)
			if finding.check != "mirror-offline" {
				t.Fatalf("check = %q, want mirror-offline", finding.check)
			}
			if finding.level != test.wantLevel {
				t.Fatalf("level = %q, want %q", finding.level, test.wantLevel)
			}
			for _, want := range test.wantParts {
				if !strings.Contains(finding.detail, want) {
					t.Fatalf("detail = %q, want substring %q", finding.detail, want)
				}
			}
		})
	}
}

// TestStatusHeadsTheListingWithTheOfflineBanner covers the wiring: `tbx status`
// asks for the mode after the listing and prints it before.
func TestStatusHeadsTheListingWithTheOfflineBanner(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`[]`)},
		{OK: true, Data: json.RawMessage(`{"enabled":true}`)},
	})

	if err := command.run([]string{"status"}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"status", "mirror.offline.get"} {
		if request := <-requests; request.Op != want {
			t.Fatalf("request op = %q, want %q", request.Op, want)
		}
	}
	if out := command.out.(*bytes.Buffer).String(); !strings.HasPrefix(out, mirrorOfflineNotice) {
		t.Fatalf("status output = %q, want it to open with the offline banner", out)
	}
}

// The status banner heads the listing while offline mode is on, and stays out
// of the way otherwise — including when the daemon could not answer.
func TestPrintMirrorOfflineNotice(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		err     error
		want    string
	}{
		{name: "on", enabled: true, want: mirrorOfflineNotice + "\n\n"},
		{name: "off", enabled: false, want: ""},
		{name: "unanswerable", enabled: true, err: errors.New("no daemon"), want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := printMirrorOfflineNotice(&out, test.enabled, test.err); err != nil {
				t.Fatal(err)
			}
			if out.String() != test.want {
				t.Fatalf("notice = %q, want %q", out.String(), test.want)
			}
		})
	}
}
