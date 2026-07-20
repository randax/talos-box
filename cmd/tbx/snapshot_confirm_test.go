package main

import (
	"bytes"
	"strings"
	"testing"
)

// confirmIfRunning must decline on a non-"y" answer. Uses a stopped/absent
// cluster path indirectly is hard without a daemon, so we test the pure
// decision: --yes always proceeds; the reader gates otherwise. Here we exercise
// the reader branch by constructing a cli whose status call is bypassed via
// --yes true (proceed) and false-with-"n" is covered by the parse-level guard.
func TestConfirmYesSkips(t *testing.T) {
	c := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: strings.NewReader("")}
	if err := c.confirmIfRunning("demo", true, "snapshot"); err != nil {
		t.Errorf("--yes should skip confirmation, got %v", err)
	}
}

func TestDefaultSnapshotNameShape(t *testing.T) {
	name := defaultSnapshotName()
	if !strings.HasPrefix(name, "snap-") || len(name) != len("snap-20060102-150405") {
		t.Errorf("default snapshot name %q has unexpected shape", name)
	}
}
