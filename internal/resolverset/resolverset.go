// Package resolverset plans the set of per-domain macOS resolver files
// (/etc/resolver/<domain>) for clusters with custom domains. The shared
// default-suffix file is not managed here; it is static and installed once.
// Only files carrying the ownership marker are ever removed, so a user's own
// resolver files are never touched (SPEC §11).
package resolverset

import (
	"fmt"
	"sort"
	"strings"
)

// Marker is the ownership line every talosbox-written resolver file starts
// with; deletion is gated on it.
const Marker = "# managed by talosbox"

// Content is the resolver file body for a custom cluster domain.
func Content(port int) string {
	return fmt.Sprintf("%s\nnameserver 127.0.0.1\nport %d\n", Marker, port)
}

// Managed reports whether an observed file carries the ownership marker.
func Managed(content []byte) bool {
	return strings.HasPrefix(string(content), Marker)
}

// Plan compares the wanted custom domains against the observed files
// (name → content) and returns the file names to (re)create and to remove,
// sorted. Unmarked files are never scheduled for removal.
func Plan(customDomains []string, observed map[string][]byte, port int) (create, remove []string) {
	want := Content(port)
	wanted := make(map[string]bool, len(customDomains))
	for _, domain := range customDomains {
		wanted[domain] = true
		if string(observed[domain]) != want {
			create = append(create, domain)
		}
	}
	for name, content := range observed {
		if !wanted[name] && Managed(content) {
			remove = append(remove, name)
		}
	}
	sort.Strings(create)
	sort.Strings(remove)
	return create, remove
}
