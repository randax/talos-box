//go:build darwin

package helper

import (
	"strings"
	"testing"
)

// syncDomainResolvers validates every requested name before touching the
// filesystem, so refusal cases are safe to exercise against the real paths.
func TestSyncDomainResolversRefusesInvalidNames(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, domain string }{
		{"non-canonical case", "Lab.Internal"},
		{"trailing dot", "lab.internal."},
		{"path traversal", "../etc"},
		{"path separator", "a/b.test"},
		{"single label", "internal"},
		{"empty", ""},
		{"reserved shared suffix", "k8s.test"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := syncDomainResolvers([]string{c.domain}, 5399)
			if err == nil || !strings.Contains(err.Error(), "refuse") {
				t.Fatalf("syncDomainResolvers(%q) = %v, want refusal", c.domain, err)
			}
		})
	}
}

func TestSyncDomainResolversRefusesBadPort(t *testing.T) {
	t.Parallel()
	if err := syncDomainResolvers(nil, 0); err == nil {
		t.Fatal("syncDomainResolvers accepted port 0")
	}
}
