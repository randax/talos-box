package domain

import (
	"strings"
	"testing"
)

func TestValidateRejects(t *testing.T) {
	cases := []struct{ name, input string }{
		{"local is mDNS", "demo.local"},
		{"localhost", "demo.localhost"},
		{"invalid TLD", "demo.invalid"},
		{"single label", "demo"},
		{"empty", ""},
		{"non-ASCII", "bücher.test"},
		{"bad char", "demo_lab.test"},
		{"leading hyphen label", "-demo.test"},
		{"trailing hyphen label", "demo-.test"},
		{"empty label", "demo..test"},
		{"label over 63", "a123456789012345678901234567890123456789012345678901234567890123.test"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Validate(c.input, true); err == nil {
				t.Fatalf("Validate(%q) accepted, want error", c.input)
			}
		})
	}
}

func TestValidateRejectsOverlongDomain(t *testing.T) {
	long := strings.Repeat("a2345678.", 28) + "test" // 256 chars total
	if _, err := Validate(long, true); err == nil {
		t.Fatalf("Validate accepted %d-char domain, want error", len(long))
	}
}

func TestValidateCanonicalizesSafeDomain(t *testing.T) {
	got, err := Validate("Lab.Test.", false)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if got != "lab.test" {
		t.Fatalf("Validate = %q, want %q", got, "lab.test")
	}
}

func TestValidateAcceptsSafeDomainsWithoutOptIn(t *testing.T) {
	for _, input := range []string{"demo.k8s.test", "lab.internal", "cluster1.home.arpa"} {
		if _, err := Validate(input, false); err != nil {
			t.Fatalf("Validate(%q, false) rejected safe domain: %v", input, err)
		}
	}
}

func TestValidateGatesUnsafeDomains(t *testing.T) {
	if _, err := Validate("corp.example.com", false); err == nil {
		t.Fatal("Validate accepted unsafe domain without opt-in")
	}
	got, err := Validate("corp.example.com", true)
	if err != nil {
		t.Fatalf("Validate rejected unsafe domain despite opt-in: %v", err)
	}
	if got != "corp.example.com" {
		t.Fatalf("Validate = %q, want %q", got, "corp.example.com")
	}
}
