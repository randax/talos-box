package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A socket-activated tbxd is never spawned by the CLI, so nothing else creates
// tbxd.log; the daemon must write it itself or `tbx logs` has no file to read.
func TestRouteDaemonLogWritesTheFileAndKeepsStderr(t *testing.T) {
	restore := swapDefaultLoggerOutput(t)
	defer restore()

	path := filepath.Join(t.TempDir(), "state", daemonLogFile)
	var closer interface{ Close() error }
	stderr := stderrDuringRouting(t, func() {
		var err error
		closer, err = routeDaemonLog(path)
		if err != nil {
			t.Error(err)
			return
		}
		log.Printf("node.start qa/qa-cp-1: VM started")
	})
	if closer == nil {
		t.Fatal("routeDaemonLog returned no closer")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read daemon log: %v", err)
	}
	if !strings.Contains(string(content), "node.start qa/qa-cp-1: VM started") {
		t.Fatalf("daemon log = %q, want the narration line", content)
	}
	if !strings.Contains(stderr, "node.start qa/qa-cp-1: VM started") {
		t.Fatalf("stderr = %q, want the narration line too so the journal stays complete", stderr)
	}
}

// On the CLI spawn path stderr already *is* tbxd.log; teeing there would write
// every line twice.
func TestRouteDaemonLogDoesNotDuplicateWhenStderrIsTheLog(t *testing.T) {
	restore := swapDefaultLoggerOutput(t)
	defer restore()

	path := filepath.Join(t.TempDir(), daemonLogFile)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = file
	log.SetOutput(file)
	closer, err := routeDaemonLog(path)
	os.Stderr = original
	if err != nil {
		t.Fatal(err)
	}
	log.Printf("balloon qa/qa-cp-1: reclaimed")
	log.SetOutput(os.Stderr)
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(content), "balloon qa/qa-cp-1: reclaimed"); got != 1 {
		t.Fatalf("line count = %d, want 1; log = %q", got, content)
	}
}

// A daemon that cannot open its log still serves.
func TestStartDaemonLogSurvivesAFailure(t *testing.T) {
	restore := swapDefaultLoggerOutput(t)
	defer restore()

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".talosbox"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	if closer := startDaemonLog(); closer != nil {
		_ = closer.Close()
		t.Fatal("closer, want none when the log could not be opened")
	}
}

// swapDefaultLoggerOutput restores the standard logger's writer after a test
// that repoints it.
func swapDefaultLoggerOutput(t *testing.T) func() {
	t.Helper()
	original := log.Writer()
	return func() { log.SetOutput(original) }
}
