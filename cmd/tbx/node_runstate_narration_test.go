package main

import (
	"strings"
	"testing"
)

// node stop / node start narrate their phases like every other state-changing
// node verb, and --quiet drops the narration without losing the result (#414).
func TestNodeRunStateNarratesTheDaemonStages(t *testing.T) {
	for _, test := range []struct {
		verb   string
		stage  string
		result string
	}{
		{verb: "stop", stage: "stopping node demo-worker-2", result: "stopped node demo-worker-2 in cluster demo"},
		{verb: "start", stage: "starting node demo-worker-2", result: "started node demo-worker-2 in cluster demo"},
	} {
		t.Run(test.verb, func(t *testing.T) {
			requests, output := runNarratingCLI(t, []narratedExchange{
				{data: `{"protocolVersion":8}`},
				{
					stages: []string{test.stage, "nodes are booting; watch them converge with: tbx status demo"},
					data:   `{"name":"demo-worker-2"}`,
				},
			}, func(command cli) error {
				return command.runNode([]string{test.verb, "demo", "demo-worker-2"})
			})

			if !requests[1].Progress {
				t.Fatalf("node.%s did not ask the daemon for progress", test.verb)
			}
			for _, wanted := range []string{test.stage, "tbx status demo", test.result} {
				if !strings.Contains(output, wanted) {
					t.Fatalf("node %s output missing %q:\n%s", test.verb, wanted, output)
				}
			}
			if strings.Index(output, test.stage) > strings.Index(output, test.result) {
				t.Fatalf("node %s printed its stages after the result:\n%s", test.verb, output)
			}
		})
	}
}

func TestNodeRunStateQuietSuppressesNarration(t *testing.T) {
	requests, output := runNarratingCLI(t, []narratedExchange{
		{data: `{"protocolVersion":8}`},
		{stages: []string{"stopping node demo-worker-2"}, data: `{"name":"demo-worker-2"}`},
	}, func(command cli) error {
		return command.runNode([]string{"stop", "demo", "demo-worker-2", "--quiet"})
	})

	if requests[1].Progress {
		t.Fatal("quiet node.stop still asked the daemon for progress")
	}
	if strings.Contains(output, "stopping node demo-worker-2") {
		t.Fatalf("quiet node stop printed narration:\n%s", output)
	}
	if !strings.Contains(output, "stopped node demo-worker-2 in cluster demo") {
		t.Fatalf("quiet node stop lost its result:\n%s", output)
	}
}
