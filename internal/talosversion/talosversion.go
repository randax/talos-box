// Package talosversion pins the Talos versions tbx supports and validates
// requested versions against that window. It sits below both the daemon and
// the provisioner so each can share the single source of truth.
package talosversion

import (
	"fmt"
	"regexp"
	"strconv"
)

// Default is the Talos version tbx boots and CI tests by default.
const Default = "v1.13.9"

// Min is the oldest Talos version tbx supports: the default's previous
// minor. Bumping Default to a new minor drags Min up in the same diff.
const Min = "v1.12.0"

// Machinery's contract parse validates nothing beyond a leading
// vMAJOR.MINOR, so image resolution needs the full triple checked here.
// Pre-release identifiers are dot-separated and never empty, per semver.
var versionShape = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

var (
	floor  = mustParse(Min)
	tested = mustParse(Default)
)

// Validate refuses malformed versions and versions below Min.
func Validate(version string) error {
	requested, err := parse(version)
	if err != nil {
		return err
	}
	if requested.less(floor) {
		return fmt.Errorf("talos version %s is below the minimum supported %s", version, Min)
	}
	return nil
}

// NewerThanTestedWarning returns the one-line create warning for a version
// above the tested Default, and "" for anything at or below it (malformed
// versions included — Validate owns rejecting those).
func NewerThanTestedWarning(version string) string {
	requested, err := parse(version)
	if err != nil {
		return ""
	}
	if tested.less(requested) {
		return fmt.Sprintf("talos %s is newer than the last tested %s; proceeding", version, Default)
	}
	return ""
}

type parsedVersion struct {
	major, minor, patch int
	prerelease          bool
}

func parse(version string) (parsedVersion, error) {
	matches := versionShape.FindStringSubmatch(version)
	if matches == nil {
		return parsedVersion{}, fmt.Errorf("invalid talos version %q: expected vMAJOR.MINOR.PATCH, like %s", version, Default)
	}
	var parsed parsedVersion
	var err error
	for _, field := range []struct {
		target *int
		digits string
	}{{&parsed.major, matches[1]}, {&parsed.minor, matches[2]}, {&parsed.patch, matches[3]}} {
		if *field.target, err = strconv.Atoi(field.digits); err != nil {
			return parsedVersion{}, fmt.Errorf("invalid talos version %q: %w", version, err)
		}
	}
	parsed.prerelease = matches[4] != ""
	return parsed, nil
}

func mustParse(version string) parsedVersion {
	parsed, err := parse(version)
	if err != nil {
		panic(err)
	}
	return parsed
}

func (v parsedVersion) less(other parsedVersion) bool {
	if v.major != other.major {
		return v.major < other.major
	}
	if v.minor != other.minor {
		return v.minor < other.minor
	}
	if v.patch != other.patch {
		return v.patch < other.patch
	}
	// Semver orders a pre-release below its release.
	return v.prerelease && !other.prerelease
}
