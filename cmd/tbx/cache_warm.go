package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/randax/talos-box/internal/daemon"
)

type warmListEntry struct {
	Source string `json:"source"`
	Line   int    `json:"line"`
	Ref    string `json:"ref"`
}

func (c cli) runCacheWarm(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tbx cache warm <list-file> [<list-file>...]")
	}
	entries, err := parseWarmListEntries(args, c.in)
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
	var result daemon.CacheWarmResult
	if err := c.call("cache.warm", daemon.CacheWarmArgs{Refs: refs}, &result); err != nil {
		return err
	}
	for _, entry := range result.Entries {
		switch entry.Status {
		case daemon.CacheWarmStatusWarmed:
			if _, err := fmt.Fprintf(c.out, "\u2713 %s warmed\n", entry.Ref); err != nil {
				return err
			}
		case daemon.CacheWarmStatusAlreadyComplete:
			if _, err := fmt.Fprintf(c.out, "\u2713 %s already complete\n", entry.Ref); err != nil {
				return err
			}
		case daemon.CacheWarmStatusFailed:
			if _, err := fmt.Fprintf(c.out, "\u2717 %s %s\n", entry.Ref, entry.Reason); err != nil {
				return err
			}
		default:
			return fmt.Errorf("cache warm returned unknown status %q for %s", entry.Status, entry.Ref)
		}
	}
	if _, err := fmt.Fprintf(c.out, "summary: %d warmed, %d already complete, %d failed\n", result.Warmed, result.AlreadyComplete, result.Failed); err != nil {
		return err
	}
	if result.Failed > 0 {
		return fmt.Errorf("cache warm failed for %d ref(s)", result.Failed)
	}
	return nil
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
