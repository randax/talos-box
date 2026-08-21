package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// destroy --force used to print one line and nothing else, giving the operator
// nothing to check the scope of the destruction against (#422).
func TestDestroyClusterPrintsASummaryOfWhatWasRemoved(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"warning":"destroying cluster demo will permanently delete 2 longhorn volumes and their data","volumes":2,"csi":"longhorn"}`)},
		{OK: true, Data: json.RawMessage(`{"name":"demo","nodes":3,"snapshots":1,"diskBytes":3221225472,"domain":"demo.example.test","resolverWithdrawn":true}`)},
	})

	if err := command.destroyCluster([]string{"demo", "--force"}); err != nil {
		t.Fatal(err)
	}

	out := command.out.(*bytes.Buffer).String()
	for _, wanted := range []string{
		"destroyed cluster demo",
		"3 node(s) removed",
		"1 snapshot(s) deleted",
		"3.0 GiB of cluster state removed",
		"3221225472 bytes",
		"resolver entry for demo.example.test withdrawn",
		"2 longhorn volume(s)",
	} {
		if !strings.Contains(out, wanted) {
			t.Fatalf("destroy summary missing %q:\n%s", wanted, out)
		}
	}
	// The byte figure is a per-file allocated-block sum, and node disks are
	// APFS clones of the image cache the destroy never touches, so calling it
	// "reclaimed" promises capacity the host does not get back.
	if strings.Contains(out, "reclaimed") {
		t.Fatalf("destroy summary claimed the disk figure was reclaimed capacity:\n%s", out)
	}
}

// A cluster on the default domain has no resolver file of its own, so the
// summary reports the DNS records that went instead of claiming otherwise.
func TestDestroyClusterSummaryOmitsResolverLineOnTheDefaultDomain(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{}`)},
		{OK: true, Data: json.RawMessage(`{"name":"demo","nodes":1,"domain":"demo.k8s.test"}`)},
	})

	if err := command.destroyCluster([]string{"demo", "--force"}); err != nil {
		t.Fatal(err)
	}

	out := command.out.(*bytes.Buffer).String()
	if strings.Contains(out, "resolver entry") {
		t.Fatalf("destroy summary claimed a resolver entry it did not withdraw:\n%s", out)
	}
	if !strings.Contains(out, "DNS records for demo.k8s.test withdrawn") {
		t.Fatalf("destroy summary missing the DNS line:\n%s", out)
	}
	if strings.Contains(out, "volume(s)") {
		t.Fatalf("destroy summary invented a volume line:\n%s", out)
	}
}

// A partially-destroyed cluster has no countable node total, and printing it
// as "0 node(s) removed" next to gigabytes of removed state would tell the
// operator the opposite of what the destroy did (#422).
func TestDestroyClusterSummaryOmitsTheNodeLineWhenTheCountIsUnknown(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{}`)},
		{OK: true, Data: json.RawMessage(`{"name":"demo","diskBytes":3221225472}`)},
	})

	if err := command.destroyCluster([]string{"demo", "--force"}); err != nil {
		t.Fatal(err)
	}

	out := command.out.(*bytes.Buffer).String()
	if strings.Contains(out, "node(s) removed") {
		t.Fatalf("destroy summary reported a node count it does not have:\n%s", out)
	}
	if !strings.Contains(out, "3.0 GiB of cluster state removed") {
		t.Fatalf("destroy summary missing the state line:\n%s", out)
	}
}

// A genuinely empty cluster still reports its zero, since the count is known.
func TestDestroyClusterSummaryKeepsAKnownZeroNodeCount(t *testing.T) {
	_, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{}`)},
		{OK: true, Data: json.RawMessage(`{"name":"demo","nodes":0,"diskBytes":4096}`)},
	})

	if err := command.destroyCluster([]string{"demo", "--force"}); err != nil {
		t.Fatal(err)
	}

	if out := command.out.(*bytes.Buffer).String(); !strings.Contains(out, "0 node(s) removed") {
		t.Fatalf("destroy summary dropped a known zero node count:\n%s", out)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, test := range []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{3221225472, "3.0 GiB"},
	} {
		if got := humanBytes(test.bytes); got != test.want {
			t.Errorf("humanBytes(%d) = %q, want %q", test.bytes, got, test.want)
		}
	}
}
