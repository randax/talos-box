package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// console evidence has to be capturable without backgrounding the process and
// killing it on a timer, so --no-follow dumps the ring buffer and returns
// (#410).
func TestDumpConsoleWritesTheReplayAndReturns(t *testing.T) {
	client, guest := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = guest.Close() })
	go func() {
		_, _ = io.WriteString(guest, "[talos] boot\n[talos] ready\n")
	}()

	var out bytes.Buffer
	if err := dumpConsole(client, &out, 0, 50*time.Millisecond, time.Second); err != nil {
		t.Fatalf("dumpConsole: %v", err)
	}
	if got := out.String(); got != "[talos] boot\n[talos] ready\n" {
		t.Fatalf("dumpConsole wrote %q, want the whole replay", got)
	}
}

// A guest that never falls silent must still let the command return.
func TestDumpConsoleStopsAtItsLimit(t *testing.T) {
	client, guest := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = guest.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := io.WriteString(guest, "chatter\n"); err != nil {
				return
			}
		}
	}()

	var out bytes.Buffer
	start := time.Now()
	if err := dumpConsole(client, &out, 0, time.Second, 200*time.Millisecond); err != nil {
		t.Fatalf("dumpConsole: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("dumpConsole took %s against a chatty guest, want its limit to end it", elapsed)
	}
	if !strings.Contains(out.String(), "chatter") {
		t.Fatal("dumpConsole returned nothing from a chatty guest")
	}
	_ = client.Close()
	<-done
}

func TestTailLines(t *testing.T) {
	for _, test := range []struct {
		name  string
		data  string
		count int
		want  string
	}{
		{name: "zero keeps everything", data: "a\nb\nc\n", count: 0, want: "a\nb\nc\n"},
		{name: "keeps the last lines", data: "a\nb\nc\n", count: 2, want: "b\nc\n"},
		{name: "more than there are keeps everything", data: "a\nb\n", count: 9, want: "a\nb\n"},
		{name: "unterminated last line counts", data: "a\nb\nc", count: 1, want: "c"},
		{name: "empty stays empty", data: "", count: 3, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := string(tailLines([]byte(test.data), test.count)); got != test.want {
				t.Fatalf("tailLines(%q, %d) = %q, want %q", test.data, test.count, got, test.want)
			}
		})
	}
}

func TestParseConsoleOptions(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		want    consoleOptions
		wantErr string
	}{
		{
			name: "follows by default",
			args: []string{"demo", "demo-cp-1"},
			want: consoleOptions{cluster: "demo", node: "demo-cp-1"},
		},
		{
			name: "no-follow with a line bound",
			args: []string{"demo", "demo-cp-1", "--no-follow", "--lines", "20"},
			want: consoleOptions{cluster: "demo", node: "demo-cp-1", noFollow: true, lines: 20},
		},
		{
			name:    "lines needs no-follow",
			args:    []string{"demo", "demo-cp-1", "--lines", "20"},
			wantErr: "--lines applies to --no-follow",
		},
		{
			name:    "since says why it is not there",
			args:    []string{"demo", "demo-cp-1", "--no-follow", "--since", "5m"},
			wantErr: "no host timestamps",
		},
		{
			name:    "still needs a cluster and a node",
			args:    []string{"demo", "--no-follow"},
			wantErr: consoleUsage,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			got, err := parseConsoleOptions(test.args, &output)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseConsoleOptions() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseConsoleOptions() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseConsoleOptions() = %+v, want %+v", got, test.want)
			}
		})
	}
}

// `tbx console --help` used to print the bare two-argument usage and nothing
// about the flags that exist (#410).
func TestConsoleHelpListsItsFlags(t *testing.T) {
	var output bytes.Buffer
	_, err := parseConsoleOptions([]string{"--help"}, &output)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("console --help error = %v, want flag.ErrHelp", err)
	}
	for _, wanted := range []string{"-no-follow", "-lines", consoleUsage} {
		if !strings.Contains(output.String(), wanted) {
			t.Fatalf("console --help output missing %q:\n%s", wanted, output.String())
		}
	}
}
