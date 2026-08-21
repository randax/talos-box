package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/provision"
)

type warmListEntry struct {
	Source string `json:"source"`
	Line   int    `json:"line"`
	Ref    string `json:"ref"`
}

func (c cli) runCacheWarm(args []string) error {
	flags := flag.NewFlagSet("cache warm", flag.ContinueOnError)
	flags.SetOutput(c.err)
	checkOnly := flags.Bool("check", false, "verify the warmed cache offline instead of downloading")
	deep := flags.Bool("deep", false, "rehash cached blobs while checking")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) == 0 {
		return errors.New("usage: tbx cache warm [--check [--deep]] <list-file> [<list-file>...]")
	}
	if *deep && !*checkOnly {
		return errors.New("cache warm --deep requires --check")
	}
	entries, err := parseWarmListEntries(positionals, c.in)
	if err != nil {
		return err
	}
	refs := make([]string, 0, len(entries))
	for _, entry := range entries {
		refs = append(refs, entry.Ref)
	}
	if len(refs) == 0 {
		return errors.New("cache warm list is empty")
	}
	// Every check is an offline-readiness verification, so it also answers for
	// the images no warm list can name: nothing tbx renders references the CRI
	// pod sandbox image, yet no pod starts without it. Adding it in both modes
	// means the gap is reported while it can still be pulled, and a plain
	// --check never hands out an all-clear a deep check would fail (#404).
	if *checkOnly {
		refs = withBootstrapRequiredRefs(refs)
	}
	if err := c.ensureCacheWarmSupport(); err != nil {
		return err
	}
	if *checkOnly {
		bootstrapRequired := make(map[string]bool)
		for _, ref := range provision.BootstrapRequiredImages() {
			bootstrapRequired[ref] = true
		}
		var complete, failed int
		for _, ref := range refs {
			var result daemon.CacheCheckResult
			if err := c.call("cache.check", daemon.CacheCheckArgs{Refs: []string{ref}, Deep: *deep}, &result); err != nil {
				return err
			}
			entry, err := cacheCheckEntryForRef(ref, result)
			if err != nil {
				return err
			}
			switch entry.Status {
			case daemon.CacheCheckStatusComplete:
				if _, err := fmt.Fprintf(c.out, "\u2713 %s complete\n", entry.Ref); err != nil {
					return err
				}
				complete++
			case daemon.CacheCheckStatusFailed:
				if _, err := fmt.Fprintf(c.out, "\u2717 %s %s\n", entry.Ref, entry.Reason); err != nil {
					return err
				}
				// The bootstrap-required refs are the ones this check adds on
				// its own, and `cache warm` has no way to close the gap it just
				// reported: nothing tbx renders references the CRI pod sandbox
				// image, so no warm list names it and the non-check path never
				// pulls it. Naming the verb that does is the whole point of
				// reporting it here (#348).
				if bootstrapRequired[entry.Ref] {
					if _, err := fmt.Fprintf(c.out, "  %s is the CRI pod sandbox image every node needs and no warm list names: run `tbx cache pull` online to cache it\n", entry.Ref); err != nil {
						return err
					}
				}
				failed++
			}
		}
		if _, err := fmt.Fprintf(c.out, "summary: %d complete, %d failed\n", complete, failed); err != nil {
			return err
		}
		if failed > 0 {
			return fmt.Errorf("cache check failed for %d ref(s)", failed)
		}
		return nil
	}
	var warmed, alreadyComplete, failed, reResolved int
	for _, ref := range refs {
		var result daemon.CacheWarmResult
		if err := c.call("cache.warm", daemon.CacheWarmArgs{Refs: []string{ref}}, &result); err != nil {
			return err
		}
		entry, err := cacheWarmEntryForRef(ref, result)
		if err != nil {
			return err
		}
		if entry.ReResolvedTag {
			reResolved++
		}
		switch entry.Status {
		case daemon.CacheWarmStatusWarmed:
			if _, err := fmt.Fprintf(c.out, "\u2713 %s warmed\n", entry.Ref); err != nil {
				return err
			}
			warmed++
		case daemon.CacheWarmStatusAlreadyComplete:
			if _, err := fmt.Fprintf(c.out, "\u2713 %s already complete\n", entry.Ref); err != nil {
				return err
			}
			alreadyComplete++
		case daemon.CacheWarmStatusFailed:
			if _, err := fmt.Fprintf(c.out, "\u2717 %s %s\n", entry.Ref, entry.Reason); err != nil {
				return err
			}
			failed++
		}
	}
	if _, err := fmt.Fprintf(c.out, "summary: %d warmed, %d already complete, %d failed\n", warmed, alreadyComplete, failed); err != nil {
		return err
	}
	// A tag is resolved upstream on every run, so a re-warm of a complete list
	// costs a full run's wall time while downloading nothing. Saying so keeps
	// the cost explainable and points at the gate that is actually cheap (#405).
	if reResolved > 0 {
		downloads := fmt.Sprintf("%d ref(s) needed downloads", warmed)
		if warmed == 0 {
			downloads = "nothing was downloaded"
		}
		if _, err := fmt.Fprintf(c.out,
			"note: re-resolved %d tag(s) upstream for freshness; %s. For a cheap offline-readiness gate run `tbx cache warm --check` instead\n",
			reResolved, downloads); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("cache warm failed for %d ref(s)", failed)
	}
	return nil
}

// withBootstrapRequiredRefs appends the bootstrap-required images the list
// does not already carry, preserving the list's own order so the appended refs
// read as the addition they are.
func withBootstrapRequiredRefs(refs []string) []string {
	present := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		present[ref] = struct{}{}
	}
	for _, ref := range provision.BootstrapRequiredImages() {
		if _, duplicate := present[ref]; duplicate {
			continue
		}
		present[ref] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

func cacheCheckEntryForRef(ref string, result daemon.CacheCheckResult) (daemon.CacheCheckEntry, error) {
	if len(result.Entries) != 1 {
		return daemon.CacheCheckEntry{}, fmt.Errorf("cache check returned %d entries for %s, want 1", len(result.Entries), ref)
	}
	entry := result.Entries[0]
	if entry.Ref != ref {
		return daemon.CacheCheckEntry{}, fmt.Errorf("cache check returned result for %s, want %s", entry.Ref, ref)
	}
	switch entry.Status {
	case daemon.CacheCheckStatusComplete:
		if result.Complete != 1 || result.Failed != 0 {
			return daemon.CacheCheckEntry{}, fmt.Errorf("cache check returned inconsistent counts for %s", ref)
		}
	case daemon.CacheCheckStatusFailed:
		if result.Complete != 0 || result.Failed != 1 {
			return daemon.CacheCheckEntry{}, fmt.Errorf("cache check returned inconsistent counts for %s", ref)
		}
	default:
		return daemon.CacheCheckEntry{}, fmt.Errorf("cache check returned unknown status %q for %s", entry.Status, entry.Ref)
	}
	return entry, nil
}

func cacheWarmEntryForRef(ref string, result daemon.CacheWarmResult) (daemon.CacheWarmEntry, error) {
	if len(result.Entries) != 1 {
		return daemon.CacheWarmEntry{}, fmt.Errorf("cache warm returned %d entries for %s, want 1", len(result.Entries), ref)
	}
	entry := result.Entries[0]
	if entry.Ref != ref {
		return daemon.CacheWarmEntry{}, fmt.Errorf("cache warm returned result for %s, want %s", entry.Ref, ref)
	}
	switch entry.Status {
	case daemon.CacheWarmStatusWarmed:
		if result.Warmed != 1 || result.AlreadyComplete != 0 || result.Failed != 0 {
			return daemon.CacheWarmEntry{}, fmt.Errorf("cache warm returned inconsistent counts for %s", ref)
		}
	case daemon.CacheWarmStatusAlreadyComplete:
		if result.Warmed != 0 || result.AlreadyComplete != 1 || result.Failed != 0 {
			return daemon.CacheWarmEntry{}, fmt.Errorf("cache warm returned inconsistent counts for %s", ref)
		}
	case daemon.CacheWarmStatusFailed:
		if result.Warmed != 0 || result.AlreadyComplete != 0 || result.Failed != 1 {
			return daemon.CacheWarmEntry{}, fmt.Errorf("cache warm returned inconsistent counts for %s", ref)
		}
	default:
		return daemon.CacheWarmEntry{}, fmt.Errorf("cache warm returned unknown status %q for %s", entry.Status, entry.Ref)
	}
	return entry, nil
}

func parseWarmListEntries(paths []string, stdin io.Reader) ([]warmListEntry, error) {
	var entries []warmListEntry
	var problems []string
	stdinUsed := false
	for _, path := range paths {
		if path == "-" {
			if stdinUsed {
				continue
			}
			stdinUsed = true
			currentEntries, currentProblems := parseWarmListSource("stdin", stdin)
			entries = append(entries, currentEntries...)
			problems = append(problems, currentProblems...)
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		currentEntries, currentProblems := parseWarmListSource(path, file)
		_ = file.Close()
		entries = append(entries, currentEntries...)
		problems = append(problems, currentProblems...)
	}
	if len(problems) != 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	return entries, nil
}

func parseWarmListSource(source string, reader io.Reader) ([]warmListEntry, []string) {
	scanner := bufio.NewScanner(reader)
	var entries []warmListEntry
	var problems []string
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if err := daemon.ValidateWarmRef(text); err != nil {
			problems = append(problems, fmt.Sprintf("%s:%d: %v", source, lineNumber, err))
			continue
		}
		entries = append(entries, warmListEntry{Source: source, Line: lineNumber, Ref: text})
	}
	if err := scanner.Err(); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %v", source, err))
	}
	return entries, problems
}
