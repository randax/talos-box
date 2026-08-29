package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

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
	jobs := flags.Int("jobs", daemon.DefaultCacheWarmJobs, "blob downloads to keep in flight (1 warms one blob at a time)")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(c.err, "Usage: tbx cache warm [--refresh] [--jobs N] <list-file> [<list-file>...]")
		_, _ = fmt.Fprintln(c.err, "       tbx cache warm --check [--deep] <list-file> [<list-file>...]")
		_, _ = fmt.Fprintln(c.err, "")
		_, _ = fmt.Fprintln(c.err, "By default, warm resumes incomplete refs and makes no upstream request for complete refs.")
		_, _ = fmt.Fprintln(c.err, "--refresh revalidates complete unpinned tags; digest-pinned refs do not need freshness resolution.")
		_, _ = fmt.Fprintln(c.err, "A transient refresh failure is nonfatal when the existing cache is complete.")
		_, _ = fmt.Fprintf(c.err, "--jobs N keeps up to N blob downloads in flight across the list (default %d); 1 warms one blob at a time.\n", daemon.DefaultCacheWarmJobs)
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
	if *jobs < 1 {
		return fmt.Errorf("cache warm --jobs must be at least 1, got %d", *jobs)
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
	var warmed, alreadyComplete, failedMissing, failedRevalidate int
	var refreshWarnings []string
	for _, next := range c.warmRefsConcurrently(refs, *refresh, *jobs) {
		entry, err := next()
		if err != nil {
			return err
		}
		switch entry.Status {
		case daemon.CacheWarmStatusWarmed:
			if _, err := fmt.Fprintf(c.out, "\u2713 %s warmed\n", entry.Ref); err != nil {
				return err
			}
			warmed++
		case daemon.CacheWarmStatusAlreadyComplete:
			suffix := ""
			if entry.RefreshWarning != "" {
				suffix = " (" + entry.RefreshWarning + ")"
				refreshWarnings = append(refreshWarnings, entry.Ref)
			}
			if _, err := fmt.Fprintf(c.out, "\u2713 %s already complete%s\n", entry.Ref, suffix); err != nil {
				return err
			}
			alreadyComplete++
		case daemon.CacheWarmStatusFailedRevalidate:
			if _, err := fmt.Fprintf(c.out, "\u2717 %s failed (revalidate): %s\n", entry.Ref, entry.Reason); err != nil {
				return err
			}
			failedRevalidate++
		case daemon.CacheWarmStatusFailedMissing:
			if _, err := fmt.Fprintf(c.out, "\u2717 %s failed (missing): %s\n", entry.Ref, entry.Reason); err != nil {
				return err
			}
			failedMissing++
		case daemon.CacheWarmStatusFailed:
			if _, err := fmt.Fprintf(c.out, "\u2717 %s failed (missing): %s\n", entry.Ref, entry.Reason); err != nil {
				return err
			}
			failedMissing++
		}
	}
	if _, err := fmt.Fprintf(c.out, "summary: %d warmed, %d already complete, %d failed (missing)", warmed, alreadyComplete, failedMissing); err != nil {
		return err
	}
	if *refresh || failedRevalidate > 0 {
		if _, err := fmt.Fprintf(c.out, ", %d failed (revalidate)", failedRevalidate); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(c.out); err != nil {
		return err
	}
	if len(refreshWarnings) > 0 {
		if _, err := fmt.Fprintf(c.out, "note: %d complete ref(s) were not revalidated: %s\n", len(refreshWarnings), strings.Join(refreshWarnings, ", ")); err != nil {
			return err
		}
	}
	if failed := failedMissing + failedRevalidate; failed > 0 {
		return fmt.Errorf("cache warm failed for %d ref(s)", failed)
	}
	return nil
}

// warmJobShares splits jobs across at most four in-flight requests so the
// shares sum to jobs: 8 -> [2 2 2 2], 6 -> [2 2 1 1], 1 -> [1].
func warmJobShares(jobs int) []int {
	const maxRequestsInFlight = 4
	requests := min(maxRequestsInFlight, jobs)
	shares := make([]int, requests)
	for i := range shares {
		shares[i] = jobs / requests
		if i < jobs%requests {
			shares[i]++
		}
	}
	return shares
}

type warmOutcome struct {
	entry daemon.CacheWarmEntry
	err   error
}

// warmRefsConcurrently keeps a few `cache.warm` requests in flight (one ref
// each, as before) so the daemon's blob pool sees more than one ref at a
// time, and hands the outcomes back in list order: each returned func blocks
// until its ref is done, so the caller still prints ✓/✗ lines progressively.
// The in-flight requests split `jobs` between them — each slot carries its
// share, and the shares sum to jobs — so the run keeps exactly --jobs blob
// downloads in flight however many requests that takes. The first error stops
// further requests from starting. --jobs 1 keeps one request in flight, which
// is exactly the serial behaviour it replaces (#506).
func (c cli) warmRefsConcurrently(refs []string, refresh bool, jobs int) []func() (daemon.CacheWarmEntry, error) {
	outcomes := make([]chan warmOutcome, len(refs))
	for i := range outcomes {
		outcomes[i] = make(chan warmOutcome, 1)
	}
	shares := warmJobShares(jobs)
	slots := make(chan int, len(shares))
	for _, share := range shares {
		slots <- share
	}
	stop := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		for i, ref := range refs {
			var share int
			select {
			case <-stop:
				return
			case share = <-slots:
			}
			select {
			case <-stop:
				// a slot freed by the failing ref is not a reason to start
				return
			default:
			}
			go func() {
				defer func() { slots <- share }()
				var result daemon.CacheWarmResult
				err := c.call("cache.warm", daemon.CacheWarmArgs{Refs: []string{ref}, Refresh: refresh, Jobs: share}, &result)
				var entry daemon.CacheWarmEntry
				if err == nil {
					entry, err = cacheWarmEntryForRef(ref, result)
				}
				if err != nil {
					// closed before this slot is returned, so it cannot be
					// handed to a ref that will never be printed
					stopOnce.Do(func() { close(stop) })
				}
				outcomes[i] <- warmOutcome{entry: entry, err: err}
			}()
		}
	}()
	next := make([]func() (daemon.CacheWarmEntry, error), len(refs))
	for i := range refs {
		next[i] = func() (daemon.CacheWarmEntry, error) {
			outcome := <-outcomes[i]
			return outcome.entry, outcome.err
		}
	}
	return next
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
	case daemon.CacheWarmStatusFailedMissing:
		if result.Warmed != 0 || result.AlreadyComplete != 0 || result.Failed != 1 || result.FailedMissing != 1 || result.FailedRevalidate != 0 {
			return daemon.CacheWarmEntry{}, fmt.Errorf("cache warm returned inconsistent counts for %s", ref)
		}
	case daemon.CacheWarmStatusFailedRevalidate:
		if result.Warmed != 0 || result.AlreadyComplete != 0 || result.Failed != 1 || result.FailedMissing != 0 || result.FailedRevalidate != 1 {
			return daemon.CacheWarmEntry{}, fmt.Errorf("cache warm returned inconsistent counts for %s", ref)
		}
	case daemon.CacheWarmStatusFailed:
		if result.Warmed != 0 || result.AlreadyComplete != 0 || result.Failed != 1 || result.FailedMissing != 0 || result.FailedRevalidate != 0 {
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
