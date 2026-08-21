package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDetachReader(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"passes plain bytes", "hello", "hello"},
		{"stops at ctrl-]", "ab\x1dcd", "ab"},
		{"ctrl-] first byte", "\x1dxyz", ""},
		{"ctrl-] at chunk boundary survives", strings.Repeat("a", 8192) + "\x1d" + "tail", strings.Repeat("a", 8192)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newDetachReader(strings.NewReader(tt.input))
			got, err := io.ReadAll(r)
			if err != nil && !errors.Is(err, errDetached) {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("read %q, want %q", got, tt.want)
			}
		})
	}
	// after the detach byte the reader must report errDetached, not io.EOF
	r := newDetachReader(strings.NewReader("x\x1d"))
	_, _ = io.ReadAll(r)
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, errDetached) {
		t.Errorf("post-detach read error = %v, want errDetached", err)
	}
}

func TestConfiguredConsoleTipIncludesEndpoint(t *testing.T) {
	t.Parallel()

	want := "tip: this node is configured — for the Talos dashboard TUI run: talosctl dashboard --talosconfig '/tmp/demo cluster/talosconfig' --nodes 172.30.0.2 --endpoints 172.30.0.2"
	if got := configuredConsoleTip("172.30.0.2", "/tmp/demo cluster/talosconfig"); got != want {
		t.Fatalf("configuredConsoleTip() = %q, want %q", got, want)
	}
}

func TestSuspendedConsoleErrorNamesAReachableCommand(t *testing.T) {
	// A node can be suspended while its siblings run (cluster suspend, then
	// node start on one node). `tbx cluster resume` refuses outright in that
	// state, so the refusal must name `tbx node start` instead (#385-#440).
	running := suspendedConsoleError("demo", "demo-cp-1", true).Error()
	if !strings.Contains(running, "tbx node start demo demo-cp-1") {
		t.Fatalf("running cluster should name node start, got %q", running)
	}
	if strings.Contains(running, "cluster resume") {
		t.Fatalf("running cluster must not name cluster resume, got %q", running)
	}
	stopped := suspendedConsoleError("demo", "demo-cp-1", false).Error()
	if !strings.Contains(stopped, "tbx cluster resume demo") {
		t.Fatalf("stopped cluster should name cluster resume, got %q", stopped)
	}
}
