package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

// daemonLogFile is the file `tbx logs` reads and every runbook points at.
const daemonLogFile = "tbxd.log"

func daemonLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".talosbox", daemonLogFile), nil
}

// routeDaemonLog points the standard logger — the daemon's whole narration — at
// path. Until this existed the only writer of tbxd.log was the CLI's spawn
// path, which redirects the child's stderr into it; a tbxd started any other
// way (socket-activated systemd user units, the documented Linux install) wrote
// solely to the journal, so `tbx logs` could never find a file to read and told
// operators to run a verb that could not create one. The daemon now owns its
// log the way it already owns the Kubernetes one.
//
// When stderr is already that same file — the CLI spawn path — the logger is
// left alone so lines are not written twice. Otherwise output is teed, keeping
// the journal complete for a supervised daemon.
func routeDaemonLog(path string) (io.Closer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if sameFile(os.Stderr, file) {
		return file, nil
	}
	log.SetOutput(io.MultiWriter(os.Stderr, file))
	return file, nil
}

// sameFile reports whether two open files are the same file on disk, so the
// spawn path's redirected stderr is recognised and not duplicated.
func sameFile(a, b *os.File) bool {
	aInfo, err := a.Stat()
	if err != nil {
		return false
	}
	bInfo, err := b.Stat()
	if err != nil {
		return false
	}
	return os.SameFile(aInfo, bInfo)
}

// startDaemonLog is the daemon's one-line wiring. Failing to open the log is
// never fatal: the daemon still serves and still narrates to stderr.
func startDaemonLog() io.Closer {
	path, err := daemonLogPath()
	if err == nil {
		var closer io.Closer
		closer, err = routeDaemonLog(path)
		if err == nil {
			return closer
		}
	}
	log.Printf("daemon log stays on stderr: %v", err)
	return nil
}
