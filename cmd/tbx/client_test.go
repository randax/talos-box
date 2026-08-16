package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonSpawnFailureTailsLog(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "tbxd.log")
	content := "2026/08/16 00:07:01 starting tbxd\n" +
		"2026/08/16 00:07:02 probe Virtualization.framework: fatal\n\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	dialErr := errors.New("dial unix ~/.talosbox/tbxd.sock: connect: no such file or directory")
	err := daemonSpawnFailure(dialErr, logPath, 0)
	for _, want := range []string{
		"tbxd was started but",
		logPath,
		"probe Virtualization.framework: fatal",
		dialErr.Error(),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("daemonSpawnFailure() = %q, missing %q", err.Error(), want)
		}
	}
}

func TestDaemonSpawnFailureIgnoresLinesFromPreviousRuns(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "tbxd.log")
	previous := "2026/08/15 10:00:00 stale error from an earlier run\n"
	if err := os.WriteFile(logPath, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}

	err := daemonSpawnFailure(errors.New("connection refused"), logPath, int64(len(previous)))
	if strings.Contains(err.Error(), "stale error") {
		t.Errorf("daemonSpawnFailure() = %q, quotes a line from a previous run", err.Error())
	}
	if strings.Contains(err.Error(), "last log line") {
		t.Errorf("daemonSpawnFailure() = %q, claims a log line despite none being written", err.Error())
	}
}

func TestDaemonSpawnFailureWithoutLog(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "tbxd.log")
	err := daemonSpawnFailure(errors.New("connection refused"), logPath, 0)
	for _, want := range []string{"tbxd was started but", logPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("daemonSpawnFailure() = %q, missing %q", err.Error(), want)
		}
	}
}

func TestDaemonSpawnFailureWithoutLogPath(t *testing.T) {
	t.Parallel()

	err := daemonSpawnFailure(errors.New("connection refused"), "", 0)
	if !strings.Contains(err.Error(), "~/.talosbox/tbxd.log") {
		t.Errorf("daemonSpawnFailure() = %q, missing display fallback path", err.Error())
	}
}

func TestLastLogLineSkipsTrailingBlanks(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "tbxd.log")
	if err := os.WriteFile(logPath, []byte("first\nsecond\n\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastLogLine(logPath, 0); got != "second" {
		t.Fatalf("lastLogLine() = %q, want %q", got, "second")
	}
	if got := lastLogLine(filepath.Join(t.TempDir(), "missing.log"), 0); got != "" {
		t.Fatalf("lastLogLine(missing) = %q, want empty", got)
	}
}

func TestLastLogLineHonorsOffset(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "tbxd.log")
	previous := "old run line\n"
	if err := os.WriteFile(logPath, []byte(previous+"new run line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastLogLine(logPath, int64(len(previous))); got != "new run line" {
		t.Fatalf("lastLogLine(offset) = %q, want %q", got, "new run line")
	}
	if got := lastLogLine(logPath, int64(len(previous)+len("new run line\n"))); got != "" {
		t.Fatalf("lastLogLine(offset at end) = %q, want empty", got)
	}
}
