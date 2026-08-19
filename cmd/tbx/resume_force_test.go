package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
)

// `cluster resume` re-admits the whole suspended footprint, so it carries the
// same --force override as create/start/node add instead of the strict arity
// check that used to reject every flag (#368).
func TestClusterResumeSendsForce(t *testing.T) {
	requests, command := newDestroyTestCLI(t, []daemon.Response{
		{OK: true, Data: json.RawMessage(`{"name":"napping","running":true}`)},
	})

	if err := command.runCluster([]string{"resume", "napping", "--force"}); err != nil {
		t.Fatal(err)
	}

	request := <-requests
	if request.Op != "cluster.resume" {
		t.Fatalf("request op = %q, want cluster.resume", request.Op)
	}
	var args struct {
		Name  string `json:"name"`
		Force bool   `json:"force"`
	}
	if err := json.Unmarshal(request.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Name != "napping" || !args.Force {
		t.Fatalf("cluster.resume args = %+v, want napping with force", args)
	}
	if out := command.out.(*bytes.Buffer).String(); !strings.Contains(out, "resumed cluster napping") {
		t.Fatalf("resume output = %q, want the resumed line", out)
	}
}

// The relaxed parse must still refuse what it does not define, and still
// require exactly one cluster name.
func TestClusterResumeRejectsBadArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown flag", args: []string{"resume", "napping", "--turbo"}, want: "not defined"},
		{name: "no name", args: []string{"resume"}, want: "usage"},
		{name: "two names", args: []string{"resume", "napping", "other"}, want: "usage"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: strings.NewReader("")}
			err := c.runCluster(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runCluster(%v) = %v, want an error mentioning %q", test.args, err, test.want)
			}
		})
	}
}
