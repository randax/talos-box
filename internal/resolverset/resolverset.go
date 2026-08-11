// Package resolverset plans the set of per-domain macOS resolver files
// (/etc/resolver/<domain>) for clusters with custom domains. The shared
// default-suffix file is not managed here; it is static and installed once.
// Files without the ownership marker are never created over, rewritten, or
// removed, so a user's own resolver files are never touched (SPEC §11).
package resolverset

import (
	"fmt"
	"sort"
	"strings"
)

// Marker is the ownership line every talosbox-written resolver file starts
// with; deletion is gated on it.
const Marker = "# managed by talosbox"

// SharedPath is the static resolver file covering every default-domain
// cluster. It predates per-domain files, carries no marker, and is installed
// and removed only by the explicit install/uninstall flow — never by Plan.
const SharedPath = "/etc/resolver/k8s.test"

// Content is the resolver file body for a custom cluster domain.
func Content(port int) string {
	return fmt.Sprintf("%s\nnameserver 127.0.0.1\nport %d\n", Marker, port)
}

// Managed reports whether an observed file carries the ownership marker as
// its exact first line — a user file that merely starts with the marker text
// (e.g. "# managed by talosbox backup") is not ours.
func Managed(content []byte) bool {
	return strings.HasPrefix(string(content), Marker+"\n")
}

// Plan compares the wanted custom domains against the observed files
// (name → content) and returns the file names to (re)create and to remove,
// sorted. Unmarked files are never touched in either direction: a wanted
// domain whose file exists without the marker is a conflict the user owns
// (doctor reports it), never an overwrite.
func Plan(customDomains []string, observed map[string][]byte, port int) (create, remove []string) {
	want := Content(port)
	wanted := make(map[string]bool, len(customDomains))
	for _, domain := range customDomains {
		wanted[domain] = true
		content, exists := observed[domain]
		if !exists || (Managed(content) && string(content) != want) {
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
