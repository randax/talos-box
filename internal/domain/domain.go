// Package domain validates cluster domains (SPEC: configurable per-cluster
// local domain). A cluster domain is chosen at create, immutable, and unique;
// see CONTEXT.md for the Cluster domain and Safe domain terms.
package domain

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
)

// Rejected TLDs collide with host resolver machinery: .local is routed to
// mDNS on macOS, .localhost and .invalid are reserved away from lookup.
var rejectedTLDs = map[string]string{
	"local":     "macOS routes .local to mDNS/Bonjour",
	"localhost": ".localhost is reserved for loopback",
	"invalid":   ".invalid is reserved to never resolve",
}

var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Validate canonicalizes input (lowercase, one trailing dot stripped) and
// applies the domain policy. Unsafe domains pass only when allowUnsafe is set.
func Validate(input string, allowUnsafe bool) (string, error) {
	name := strings.ToLower(strings.TrimSuffix(input, "."))
	if len(name) > 253 {
		return "", fmt.Errorf("domain %q is longer than 253 characters", name)
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("domain %q needs at least two labels (e.g. lab.test)", name)
	}
	for _, label := range labels {
		if len(label) > 63 {
			return "", fmt.Errorf("domain label %q is longer than 63 characters", label)
		}
		if !labelRe.MatchString(label) {
			return "", fmt.Errorf("domain label %q is invalid (lowercase letters, digits, inner hyphens)", label)
		}
	}
	if reason, rejected := rejectedTLDs[labels[len(labels)-1]]; rejected {
		return "", fmt.Errorf("domain %q is not usable: %s", name, reason)
	}
	if name == cluster.DefaultDomainSuffix {
		return "", fmt.Errorf("domain %q is reserved as the shared suffix for default cluster domains", name)
	}
	if !Safe(name) && !allowUnsafe {
		return "", fmt.Errorf("domain %q can shadow real DNS; pass --allow-unsafe-domain to use it anyway", name)
	}
	return name, nil
}

// Safe reports whether a canonical domain sits under a name reserved away
// from real DNS: .test, .internal, or home.arpa (see the Safe domain term in
// CONTEXT.md).
func Safe(name string) bool {
	switch name[strings.LastIndex(name, ".")+1:] {
	case "test", "internal":
		return true
	}
	return name == "home.arpa" || strings.HasSuffix(name, ".home.arpa")
}
