package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

func TestSnapshotListPrintsHeadedTable(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`[{"name":"baseline","created":"2026-08-17T10:00:00Z"}]`)},
	})

	if err := command.snapshotList([]string{"demo"}); err != nil {
		t.Fatal(err)
	}

	out := command.out.(*bytes.Buffer).String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("snapshot list printed %d lines, want header + row:\n%s", len(lines), out)
	}
	for _, wanted := range []string{"NAME", "CREATED"} {
		if !strings.Contains(lines[0], wanted) {
			t.Errorf("snapshot list header missing %q: %q", wanted, lines[0])
		}
	}
	if !strings.Contains(lines[1], "baseline") || !strings.Contains(lines[1], "2026-08-17 10:00") {
		t.Errorf("snapshot list row = %q, want name and created time", lines[1])
	}
}
