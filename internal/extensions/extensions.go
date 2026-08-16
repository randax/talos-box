// Package extensions defines the curated Talos system extensions talosbox
// composes into its Image Factory schematics. The set is deliberately closed:
// every member is verified by the cluster e2e, and the talos.schematic escape
// hatch covers everything outside it.
package extensions

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// curated maps the bare short name users write to the official Image Factory
// extension name. tbx owns this mapping so config files never carry vendor
// prefixes.
var curated = map[string]string{
	"gvisor":           "siderolabs/gvisor",
	"nfs-utils":        "siderolabs/nfs-utils",
	"qemu-guest-agent": "siderolabs/qemu-guest-agent",
}

// GuestAgent is the curated extension whose usefulness depends on the host
// backend exposing a guest-agent channel.
const GuestAgent = "qemu-guest-agent"

// Requested reports whether a cluster's extension list contains name.
func Requested(requested []string, name string) bool {
	for _, item := range requested {
		if item == name {
			return true
		}
	}
	return false
}

// suggestionDistance is the edit distance below which an unknown name is
// reported as a typo of a curated one.
const suggestionDistance = 3

// Names returns the curated short names, sorted.
func Names() []string {
	names := make([]string, 0, len(curated))
	for name := range curated {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Ref returns the official extension name a curated short name maps to.
func Ref(name string) (string, bool) {
	ref, ok := curated[name]
	return ref, ok
}

// Resolve validates requested short names against the curated set and returns
// their official refs, deduplicated and sorted. Sorting keeps the composed
// schematic content-addressed by the request rather than by its spelling.
// Validation is purely local: it must work with no network at all.
func Resolve(requested []string) ([]string, error) {
	seen := make(map[string]struct{}, len(requested))
	refs := make([]string, 0, len(requested))
	for _, name := range requested {
		if name == "" {
			return nil, errors.New("extension name must not be empty")
		}
		ref, ok := curated[name]
		if !ok {
			return nil, unknownExtensionError(name)
		}
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, nil
}

func unknownExtensionError(name string) error {
	message := fmt.Sprintf("unknown extension %q", name)
	if suggestion, ok := suggest(name); ok {
		message += fmt.Sprintf(": did you mean %q?", suggestion)
	} else {
		message += ":"
	}
	return fmt.Errorf("%s curated extensions: %s", message, strings.Join(Names(), ", "))
}

func suggest(name string) (string, bool) {
	best, bestDistance := "", suggestionDistance+1
	for _, candidate := range Names() {
		if len(name) >= 3 && strings.HasPrefix(candidate, name) {
			return candidate, true
		}
		if distance := editDistance(name, candidate); distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	return best, best != ""
}

// editDistance is the Levenshtein distance between two names, used only to
// decide whether an unknown name is close enough to be a typo.
func editDistance(left, right string) int {
	previous := make([]int, len(right)+1)
	current := make([]int, len(right)+1)
	for column := range previous {
		previous[column] = column
	}
	for row := 1; row <= len(left); row++ {
		current[0] = row
		for column := 1; column <= len(right); column++ {
			substitution := previous[column-1]
			if left[row-1] != right[column-1] {
				substitution++
			}
			current[column] = minimum(substitution, previous[column]+1, current[column-1]+1)
		}
		previous, current = current, previous
	}
	return previous[len(right)]
}

func minimum(values ...int) int {
	smallest := values[0]
	for _, value := range values[1:] {
		if value < smallest {
			smallest = value
		}
	}
	return smallest
}
