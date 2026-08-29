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
	refresh := flags.Bool("refresh", false, "revalidate complete unpinned tags before warming")
	jobs := flags.Int("jobs", daemon.DefaultCacheWarmJobs, fmt.Sprintf("blob downloads to keep in flight, 1-%d (1 warms one blob at a time)", daemon.MaxCacheWarmJobs))
	flags.Usage = func() {
		_, _ = fmt.Fprintln(c.err, "Usage: tbx cache warm [--refresh] [--jobs N] <list-file> [<list-file>...]")
		_, _ = fmt.Fprintln(c.err, "       tbx cache warm --check [--deep] <list-file> [<list-file>...]")
		_, _ = fmt.Fprintln(c.err, "")
		_, _ = fmt.Fprintln(c.err, "By default, warm resumes incomplete refs and makes no upstream request for complete refs.")
		_, _ = fmt.Fprintln(c.err, "--refresh revalidates complete unpinned tags; digest-pinned refs do not need freshness resolution.")
		_, _ = fmt.Fprintln(c.err, "A transient refresh failure is nonfatal when the existing cache is complete.")
		_, _ = fmt.Fprintf(c.err, "--jobs N keeps N blob downloads in flight across the list (default %d, at most %d); 1 warms one blob at a time.\n", daemon.DefaultCacheWarmJobs, daemon.MaxCacheWarmJobs)
		_, _ = fmt.Fprintln(c.err, "--check verifies tag mapping, selected linux/<arch> manifest, config, and all layers locally.")
		_, _ = fmt.Fprintln(c.err, "--deep additionally hashes cached blobs.")
	}
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) == 0 {
		return errors.New("usage: tbx cache warm [--refresh | --check [--deep]] <list-file> [<list-file>...]")
	}
	if *deep && !*checkOnly {
		return errors.New("cache warm --deep requires --check")
	}
	if *refresh && *checkOnly {
		return errors.New("cache warm --refresh cannot be used with --check")
	}
	if *jobs < 1 || *jobs > daemon.MaxCacheWarmJobs {
		return fmt.Errorf("cache warm --jobs must be between 1 and %d, got %d", daemon.MaxCacheWarmJobs, *jobs)
	}
	jobsGiven := false
	flags.Visit(func(f *flag.Flag) { jobsGiven = jobsGiven || f.Name == "jobs" })
	if jobsGiven && *checkOnly {
		return errors.New("cache warm --jobs cannot be used with --check")
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
	// means the gap is reported while it can still be pulled, whether or not
	// --deep was asked for (#404). --deep still adds what a plain --check does
	// not do: rehashing cached blobs to catch on-disk corruption.
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
	// One request carries the whole list: the daemon runs the refs under its
	// --jobs blob budget and narrates each finished entry in list order, so
	// the lines below still appear as refs complete. A daemon that does not
	// narrate entries (or narrates fewer than it answers) is covered by the
	// final result, printed from where the narration stopped (#506).
	var tally warmTally
	var printErr error
	onStage := func(stage string) {
		entry, ok := daemon.ParseCacheWarmEntryStage(stage)
		if !ok {
			_, _ = fmt.Fprintln(c.err, stage)
			return
		}
		if printErr != nil || tally.printed >= len(refs) {
			return
		}
		if entry.Ref != refs[tally.printed] {
			printErr = fmt.Errorf("cache warm narrated %s, want %s", entry.Ref, refs[tally.printed])
			return
		}
		printErr = tally.print(c.out, entry)
	}
	var result daemon.CacheWarmResult
	if err := c.callNarrated("cache.warm", daemon.CacheWarmArgs{Refs: refs, Refresh: *refresh, Jobs: *jobs}, &result, onStage); err != nil {
		return err
	}
	if printErr != nil {
		return printErr
	}
	if len(result.Entries) != len(refs) {
		return fmt.Errorf("cache warm returned %d entries for %d refs", len(result.Entries), len(refs))
	}
	for i := tally.printed; i < len(refs); i++ {
		if result.Entries[i].Ref != refs[i] {
			return fmt.Errorf("cache warm returned result for %s, want %s", result.Entries[i].Ref, refs[i])
		}
		if err := tally.print(c.out, result.Entries[i]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(c.out, "summary: %d warmed, %d already complete, %d failed (missing)", tally.warmed, tally.alreadyComplete, tally.failedMissing); err != nil {
		return err
	}
	if *refresh || tally.failedRevalidate > 0 {
		if _, err := fmt.Fprintf(c.out, ", %d failed (revalidate)", tally.failedRevalidate); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(c.out); err != nil {
		return err
	}
	if len(tally.refreshWarnings) > 0 {
		if _, err := fmt.Fprintf(c.out, "note: %d complete ref(s) were not revalidated: %s\n", len(tally.refreshWarnings), strings.Join(tally.refreshWarnings, ", ")); err != nil {
			return err
		}
	}
	if failed := tally.failedMissing + tally.failedRevalidate; failed > 0 {
		return fmt.Errorf("cache warm failed for %d ref(s)", failed)
	}
	return nil
}

// warmTally prints one warm entry per line and keeps the counts the summary
// line reports.
type warmTally struct {
	printed, warmed, alreadyComplete, failedMissing, failedRevalidate int
	refreshWarnings                                                   []string
}

func (t *warmTally) print(out io.Writer, entry daemon.CacheWarmEntry) error {
	t.printed++
	var err error
	switch entry.Status {
	case daemon.CacheWarmStatusWarmed:
		_, err = fmt.Fprintf(out, "\u2713 %s warmed\n", entry.Ref)
		t.warmed++
	case daemon.CacheWarmStatusAlreadyComplete:
		suffix := ""
		if entry.RefreshWarning != "" {
			suffix = " (" + entry.RefreshWarning + ")"
			t.refreshWarnings = append(t.refreshWarnings, entry.Ref)
		}
		_, err = fmt.Fprintf(out, "\u2713 %s already complete%s\n", entry.Ref, suffix)
		t.alreadyComplete++
	case daemon.CacheWarmStatusFailedRevalidate:
		_, err = fmt.Fprintf(out, "\u2717 %s failed (revalidate): %s\n", entry.Ref, entry.Reason)
		t.failedRevalidate++
	case daemon.CacheWarmStatusFailedMissing, daemon.CacheWarmStatusFailed:
		_, err = fmt.Fprintf(out, "\u2717 %s failed (missing): %s\n", entry.Ref, entry.Reason)
		t.failedMissing++
	}
	return err
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
