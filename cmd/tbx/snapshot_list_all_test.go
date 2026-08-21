package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// `tbx snapshot list` without a cluster is the post-destroy residue check:
// every cluster's snapshots, with the cluster named (#417).
func TestSnapshotListWithoutAClusterListsEveryCluster(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`[{"name":"baseline","created":"2026-08-17T10:00:00Z","cluster":"demo"}]`)},
	})

	if err := command.snapshotList(nil); err != nil {
		t.Fatal(err)
	}

	request := <-requests
	var args struct {
		Cluster string `json:"cluster"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Cluster != "" {
		t.Fatalf("snapshot.list args = %+v, want no cluster filter", args)
	}
	out := command.out.(*bytes.Buffer).String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "CLUSTER") {
		t.Fatalf("snapshot list output = %q, want a cluster-headed table", out)
	}
	if !strings.Contains(lines[1], "demo") || !strings.Contains(lines[1], "baseline") {
		t.Fatalf("snapshot list row = %q, want the cluster and the snapshot", lines[1])
	}
}

// With nothing left anywhere the answer is an empty result and exit 0 — the
// usage error made the cleanup question unaskable (#417).
func TestSnapshotListWithoutAClusterReportsAnEmptyResult(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`[]`)},
	})

	if err := command.snapshotList(nil); err != nil {
		t.Fatalf("snapshot list with no snapshots anywhere failed: %v", err)
	}
	if got := strings.TrimSpace(command.out.(*bytes.Buffer).String()); got != "no snapshots" {
		t.Fatalf("snapshot list stdout = %q, want the empty result", got)
	}
}

func TestSnapshotListRejectsASecondPositional(t *testing.T) {
	_, command := newDestroyTestCLI(t, nil)

	err := command.snapshotList([]string{"demo", "extra"})
	if err == nil || !strings.Contains(err.Error(), "usage: tbx snapshot list [cluster]") {
		t.Fatalf("snapshot list error = %v, want the usage refusal", err)
	}
}
