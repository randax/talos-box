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

func TestSnapshotListJSONOutput(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`[{"name":"baseline","created":"2026-08-17T10:00:00Z"}]`)},
	})

	if err := command.snapshotList([]string{"demo", "-o", "json"}); err != nil {
		t.Fatal(err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(command.out.(*bytes.Buffer).Bytes(), &decoded); err != nil {
		t.Fatalf("snapshot list -o json is not valid JSON: %v", err)
	}
	if len(decoded) != 1 || decoded[0]["name"] != "baseline" {
		t.Fatalf("snapshot list -o json = %+v, want the baseline snapshot", decoded)
	}
}

func TestSnapshotListRejectsUnknownOutputFormat(t *testing.T) {
	c := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	err := c.snapshotList([]string{"demo", "-o", "yaml"})
	if err == nil || !strings.Contains(err.Error(), `unknown output format "yaml"`) {
		t.Fatalf("snapshot list -o yaml error = %v, want the unknown-format refusal", err)
	}
}

func TestSnapshotListJSONEmitsEmptyArrayForNoSnapshots(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`null`)},
	})

	if err := command.snapshotList([]string{"demo", "-o", "json"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(command.out.(*bytes.Buffer).String()); got != "[]" {
		t.Fatalf("empty snapshot list -o json = %q, want []", got)
	}
}
