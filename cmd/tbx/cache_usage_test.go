package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// TestGroupCommandHelpPrintsUsage: `tbx <group> --help` used to error with
// unknown <group> command "--help"; it now prints the group's usage.
func TestGroupCommandHelpPrintsUsage(t *testing.T) {
	for group, usage := range groupUsages {
		for _, flag := range []string{"-h", "--help"} {
			var stdout, stderr bytes.Buffer
			command := cli{out: &stdout, err: &stderr}
			if err := command.run([]string{group, flag}); err != nil {
				t.Errorf("tbx %s %s = %v, want the usage line", group, flag, err)
				continue
			}
			if got := stdout.String(); got != usage+"\n" {
				t.Errorf("tbx %s %s stdout = %q, want %q", group, flag, got, usage+"\n")
			}
			if stderr.Len() != 0 {
				t.Errorf("tbx %s %s stderr = %q, want empty", group, flag, stderr.String())
			}
		}
	}
}

// TestGroupUsageMatchesBareInvocation keeps the help text and the bare-command
// refusal from drifting apart: both name the same verbs.
func TestGroupUsageMatchesBareInvocation(t *testing.T) {
	for group, usage := range groupUsages {
		var stdout, stderr bytes.Buffer
		command := cli{out: &stdout, err: &stderr}
		err := command.run([]string{group})
		if err == nil {
			t.Errorf("tbx %s = nil error, want a usage refusal", group)
			continue
		}
		if err.Error() != usage {
			t.Errorf("tbx %s error = %q, want %q", group, err.Error(), usage)
		}
	}
}

// TestGroupCommandHelpDoesNotSwallowVerbs keeps the help shortcut narrow: a
// real verb still dispatches, and an unknown one still errors.
func TestGroupCommandHelpDoesNotSwallowVerbs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.run([]string{"cache", "bogus"})
	if err == nil || !strings.Contains(err.Error(), `unknown cache command "bogus"`) {
		t.Fatalf("tbx cache bogus = %v, want the unknown-command error", err)
	}
}

// TestUnknownGroupVerbNamesTheValidVerbs: an unknown verb used to name only
// itself, which left `tbx node start` (before it existed) and every typo
// without a single valid verb to try (#274). The refusal now carries the
// group's usage line, so the verb list is one place away from the mistake.
func TestUnknownGroupVerbNamesTheValidVerbs(t *testing.T) {
	for _, group := range []string{"cluster", "node", "cache", "snapshot", "system", "bgp", "mirror"} {
		var stdout, stderr bytes.Buffer
		command := cli{out: &stdout, err: &stderr}
		err := command.run([]string{group, "bogus"})
		if err == nil {
			t.Errorf("tbx %s bogus = nil error, want the unknown-verb refusal", group)
			continue
		}
		for _, want := range []string{`unknown ` + group + ` command "bogus"`, groupUsages[group]} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("tbx %s bogus error = %q, want it to contain %q", group, err.Error(), want)
			}
		}
	}
}

// TestNodeRunStateVerbsDispatch: start and stop are node verbs, not unknown
// commands (#274). They are gated on the daemon handshake, so the refusal here
// is about the daemon rather than the verb.
func TestNodeRunStateVerbsDispatch(t *testing.T) {
	for _, verb := range []string{"start", "stop"} {
		var stdout, stderr bytes.Buffer
		command := cli{out: &stdout, err: &stderr}
		err := command.run([]string{"node", verb})
		if err == nil || !strings.Contains(err.Error(), "usage: tbx node "+verb+" <cluster> <node>") {
			t.Errorf("tbx node %s = %v, want the verb's own usage line", verb, err)
		}
	}
}

// TestCachePruneNamesMutualExclusivity: the refusal explains the conflict
// instead of printing a bare usage line.
func TestCachePruneNamesMutualExclusivity(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := cli{out: &stdout, err: &stderr}
	err := command.run([]string{"cache", "prune", "--mirror", "--all"})
	if err == nil {
		t.Fatal("cache prune accepted conflicting flags")
	}
	if !strings.Contains(err.Error(), "--mirror and --all are mutually exclusive") {
		t.Fatalf("error = %q, want the mutual-exclusivity message", err.Error())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// TestCacheImageLineReportsAllocatedSize: a sparse disk.raw overstates its
// footprint, so the line carries the bytes it occupies beside the apparent size.
func TestCacheImageLineReportsAllocatedSize(t *testing.T) {
	entry := daemon.CacheImageEntry{
		Schematic: "abc", Version: "v1.2.3", Architecture: "arm64",
		Size: 2355101696, AllocatedSize: 205520896,
	}
	if got, want := cacheImageLine(entry), "abc v1.2.3 arm64 2355101696 bytes (205520896 bytes on disk)"; got != want {
		t.Fatalf("cacheImageLine = %q, want %q", got, want)
	}
	// An older daemon reports no allocated size; the line stays as it was.
	entry.AllocatedSize = 0
	if got, want := cacheImageLine(entry), "abc v1.2.3 arm64 2355101696 bytes"; got != want {
		t.Fatalf("cacheImageLine without allocated size = %q, want %q", got, want)
	}
}
