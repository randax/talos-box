package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeDaemonLog points HOME at a temp dir holding a daemon log with content.
func writeDaemonLog(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".talosbox", "tbxd.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The fixture carries both subject conventions the daemon writes: node-scoped
// "<cluster>/<node>" lines and cluster-scoped "<verb> <cluster>:" ones. The
// cluster filter has to keep both, or a stalled provision's own narration is
// invisible to `tbx logs <cluster>` (#402).
const sampleDaemonLog = `2026/08/19 13:37:01 balloon qa-cil/qa-cil-cp-1: target=2048MiB
2026/08/19 13:37:02 provision qa-cil: waiting on Longhorn node scheduling: longhorn manager is not Ready
2026/08/19 13:37:03 node.remove qa-cil/qa-cil-worker-1: begin
2026/08/19 13:37:04 balloon qa-sta/qa-sta-cp-1: target=1024MiB
2026/08/19 13:37:05 storage probe qa-sta: failed: context deadline exceeded
2026/08/19 13:37:06 node.remove qa-cil/qa-cil-worker-1: complete
`

// TestLogsPrintsTheDaemonLog pins #402: the diagnostic path the docs point at
// is reachable from the CLI itself.
func TestLogsPrintsTheDaemonLog(t *testing.T) {
	writeDaemonLog(t, sampleDaemonLog)
	command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: strings.NewReader("")}

	if err := command.run([]string{"logs"}); err != nil {
		t.Fatal(err)
	}

	if out := command.out.(*bytes.Buffer).String(); out != sampleDaemonLog {
		t.Fatalf("logs output = %q, want the whole log", out)
	}
}

// TestLogsFiltersAndTails covers the cluster filter (positional and flag) and
// --lines.
func TestLogsFiltersAndTails(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		notWant []string
	}{
		{
			name: "positional cluster filter",
			args: []string{"logs", "qa-cil"},
			want: []string{
				"qa-cil/qa-cil-cp-1",
				"provision qa-cil: waiting on Longhorn node scheduling",
				"qa-cil/qa-cil-worker-1: complete",
			},
			notWant: []string{"qa-sta"},
		},
		{
			name:    "cluster flag",
			args:    []string{"logs", "--cluster", "qa-sta"},
			want:    []string{"qa-sta/qa-sta-cp-1", "storage probe qa-sta: failed"},
			notWant: []string{"qa-cil"},
		},
		{
			name:    "lines bounds the tail",
			args:    []string{"logs", "--lines", "1"},
			want:    []string{"qa-cil/qa-cil-worker-1: complete"},
			notWant: []string{"13:37:01", "13:37:02", "13:37:03", "13:37:04", "13:37:05"},
		},
		{
			name:    "filter and tail combine",
			args:    []string{"logs", "qa-cil", "--lines", "1"},
			want:    []string{"qa-cil/qa-cil-worker-1: complete"},
			notWant: []string{"13:37:01", "qa-sta"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeDaemonLog(t, sampleDaemonLog)
			command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: strings.NewReader("")}
			if err := command.run(test.args); err != nil {
				t.Fatal(err)
			}
			out := command.out.(*bytes.Buffer).String()
			for _, want := range test.want {
				if !strings.Contains(out, want) {
					t.Fatalf("logs output = %q, want substring %q", out, want)
				}
			}
			for _, unwanted := range test.notWant {
				if strings.Contains(out, unwanted) {
					t.Fatalf("logs output = %q, want no substring %q", out, unwanted)
				}
			}
		})
	}
}

// `tbx system logs` is the long spelling of the same verb.
func TestSystemLogsIsAnAliasForLogs(t *testing.T) {
	writeDaemonLog(t, sampleDaemonLog)
	command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: strings.NewReader("")}

	if err := command.run([]string{"system", "logs", "qa-sta"}); err != nil {
		t.Fatal(err)
	}

	out := command.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "qa-sta/qa-sta-cp-1") || strings.Contains(out, "qa-cil") {
		t.Fatalf("system logs output = %q, want only the qa-sta lines", out)
	}
}

// A missing log says what to do instead of failing with a bare open error.
func TestLogsExplainsAMissingDaemonLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: strings.NewReader("")}

	err := command.run([]string{"logs"})
	if err == nil || !strings.Contains(err.Error(), "no daemon log at") {
		t.Fatalf("logs error = %v, want the missing-log explanation", err)
	}
	// the hint has to name a verb that actually spawns tbxd; `tbx system
	// status` deliberately does not, so following it left logs failing
	if !strings.Contains(err.Error(), "tbx status") {
		t.Fatalf("logs error = %v, want a hint naming a daemon-backed verb", err)
	}
	if strings.Contains(err.Error(), "tbx system status") {
		t.Fatalf("logs error = %v, must not point at a command that never starts tbxd", err)
	}
	// a supervised tbxd (systemd socket activation) predating the daemon-owned
	// log writes only to the journal, so retrying can never succeed there
	if !strings.Contains(err.Error(), "journalctl --user -u tbxd") {
		t.Fatalf("logs error = %v, want the supervised-daemon case named", err)
	}
}

func TestLogsRejectsBadArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "two clusters", args: []string{"logs", "a", "b"}, want: "usage"},
		{name: "flag and positional", args: []string{"logs", "a", "--cluster", "b"}, want: "usage"},
		{name: "negative lines", args: []string{"logs", "--lines", "-1"}, want: "--lines"},
		{name: "unknown flag", args: []string{"logs", "--tail"}, want: "not defined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeDaemonLog(t, sampleDaemonLog)
			command := cli{out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: strings.NewReader("")}
			err := command.run(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run(%v) = %v, want an error mentioning %q", test.args, err, test.want)
			}
		})
	}
}

// --follow keeps printing what the daemon appends after the tail.
func TestFollowLogPrintsAppendedLines(t *testing.T) {
	path := writeDaemonLog(t, sampleDaemonLog)
	original := logFollowInterval
	logFollowInterval = time.Millisecond
	defer func() { logFollowInterval = original }()

	var out bytes.Buffer
	offset, err := printLogTail(&out, path, 0, "qa-cil")
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("2026/08/19 13:37:05 balloon qa-sta/qa-sta-cp-1: target=512MiB\n" +
		"2026/08/19 13:37:06 node.add qa-cil/qa-cil-worker-2: begin\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	followed := &syncBuffer{}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- followLog(followed, path, offset, "qa-cil", stop) }()
	deadline := time.After(5 * time.Second)
	for !strings.Contains(followed.String(), "qa-cil-worker-2") {
		select {
		case <-deadline:
			t.Fatalf("follow output = %q, want the appended qa-cil line", followed.String())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := followed.String(); strings.Contains(got, "qa-sta") {
		t.Fatalf("follow output = %q, want the filter to hold while following", got)
	}
}

// syncBuffer lets the test read what the following goroutine wrote.
type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
