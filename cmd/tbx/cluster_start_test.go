package main

import (
	"io"
	"testing"
)

func TestParseClusterStartArgsAcceptsForce(t *testing.T) {
	name, force, err := parseClusterStartArgs([]string{"demo", "--force"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if name != "demo" || !force {
		t.Fatalf("parseClusterStartArgs() = %q, %v; want demo, true", name, force)
	}
}
