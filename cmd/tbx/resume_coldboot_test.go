package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// TestClusterResumeWarnsBeforeTheSuccessLine pins #411: the summary must not be
// able to contradict the detail below it, so the per-node cold-boot warnings
// come first and the success line carries the count.
func TestClusterResumeWarnsBeforeTheSuccessLine(t *testing.T) {
	data := `{"name":"qa-sta","running":true,"warnings":[` +
		`"qa-sta-cp-1: saved state could not be restored; cold-booting instead (details: ~/.talosbox/tbxd.log)",` +
		`"qa-sta-worker-1: no saved state found; cold-booting instead",` +
		`"qa-sta-worker-2: saved state could not be restored; cold-booting instead (details: ~/.talosbox/tbxd.log)"` +
		`],"warning":"three of them"}`
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(data)},
	})
	combined := &bytes.Buffer{}
	command.out, command.err = combined, combined

	if err := command.runCluster([]string{"resume", "qa-sta"}); err != nil {
		t.Fatal(err)
	}

	output := combined.String()
	success := "resumed cluster qa-sta (3 node(s) cold-booted)"
	if !strings.Contains(output, success) {
		t.Fatalf("resume output = %q, want %q", output, success)
	}
	if index := strings.Index(output, success); index < strings.LastIndex(output, "cold-booting instead") {
		t.Fatalf("resume output = %q, want every warning before the success line", output)
	}
}

// A clean resume keeps the bare success line: the count only appears when
// something actually cold-booted.
func TestClusterResumeWithoutColdBootsKeepsTheBareLine(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"name":"qa-sta","running":true}`)},
	})

	if err := command.runCluster([]string{"resume", "qa-sta"}); err != nil {
		t.Fatal(err)
	}

	if out := command.out.(*bytes.Buffer).String(); out != "resumed cluster qa-sta\n" {
		t.Fatalf("resume output = %q, want the bare resumed line", out)
	}
}

func TestColdBootedNodeCount(t *testing.T) {
	tests := []struct {
		name     string
		warnings []string
		joined   string
		want     int
	}{
		{name: "no warnings", want: 0},
		{
			name:     "mixed warnings count only cold boots",
			warnings: []string{"host memory is tight", "a: cold-booting instead", "b: cold-booting instead"},
			want:     2,
		},
		{
			name:   "joined form from a skewed daemon",
			joined: "a: cold-booting instead; b: cold-booting instead",
			want:   2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := coldBootedNodeCount(test.warnings, test.joined); got != test.want {
				t.Fatalf("coldBootedNodeCount = %d, want %d", got, test.want)
			}
		})
	}
}
