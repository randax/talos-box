package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/term"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/shellquote"
)

// detachByte is Ctrl-] — the telnet-style detach key (SPEC §10).
const detachByte = 0x1d

var errDetached = errors.New("detached")

// detachReader passes bytes through until the detach byte, then fails with
// errDetached forever after.
type detachReader struct {
	source   io.Reader
	detached bool
}

func newDetachReader(source io.Reader) *detachReader {
	return &detachReader{source: source}
}

func (d *detachReader) Read(p []byte) (int, error) {
	if d.detached {
		return 0, errDetached
	}
	n, err := d.source.Read(p)
	if i := bytes.IndexByte(p[:n], detachByte); i >= 0 {
		d.detached = true
		return i, errDetached
	}
	return n, err
}

const consoleUsage = "usage: tbx console <cluster> <node> [--no-follow] [--lines N]"

// consoleOptions is what `tbx console` was asked to do: attach and follow, or
// dump what the node's console ring buffer already holds and exit (#410).
type consoleOptions struct {
	cluster  string
	node     string
	noFollow bool
	lines    int
}

// sinceUnsupported explains why there is no --since. The console ring buffer
// is the guest's raw byte stream — kernel and machined output as the VM wrote
// it — with no host timestamps to cut on, so a duration could only be guessed
// at from log text that Talos does not promise. --lines is the bounded form
// that the buffer can actually answer (#410).
const sinceUnsupported = "--since is not available: the console ring buffer holds the guest's raw bytes with no host timestamps to cut on; use --lines N instead"

func parseConsoleOptions(args []string, output io.Writer) (consoleOptions, error) {
	flags := flag.NewFlagSet("console", flag.ContinueOnError)
	flags.SetOutput(output)
	noFollow := flags.Bool("no-follow", false, "dump the console ring buffer and exit instead of following")
	lines := flags.Int("lines", 0, "with --no-follow, print only the last N lines (0 prints the whole buffer)")
	since := flags.String("since", "", "not supported; see --lines")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, printErr := fmt.Fprintln(output, consoleUsage)
			if printErr != nil {
				return consoleOptions{}, printErr
			}
			return consoleOptions{}, flag.ErrHelp
		}
		return consoleOptions{}, err
	}
	if len(positionals) != 2 {
		return consoleOptions{}, errors.New(consoleUsage)
	}
	if *since != "" {
		return consoleOptions{}, errors.New(sinceUnsupported)
	}
	if *lines < 0 {
		return consoleOptions{}, errors.New("--lines cannot be negative")
	}
	if *lines > 0 && !*noFollow {
		return consoleOptions{}, errors.New("--lines applies to --no-follow; a followed console replays the whole buffer")
	}
	return consoleOptions{cluster: positionals[0], node: positionals[1], noFollow: *noFollow, lines: *lines}, nil
}

func (c cli) runConsole(args []string) error {
	options, err := parseConsoleOptions(args, c.err)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	clusterName, nodeName := options.cluster, options.node

	// validate against the daemon's view (also surfaces phase for the banner)
	var statuses []daemon.ClusterStatus
	if err := c.call("status", map[string]string{"cluster": clusterName}, &statuses); err != nil {
		return err
	}
	if len(statuses) == 0 {
		return fmt.Errorf("cluster %q does not exist", clusterName)
	}
	var target *daemon.NodeStatus
	for i, node := range statuses[0].Nodes {
		if node.Name == nodeName {
			target = &statuses[0].Nodes[i]
		}
	}
	if target == nil {
		return fmt.Errorf("node %q does not exist in cluster %q", nodeName, clusterName)
	}
	if target.Phase == daemon.PhaseSuspended {
		return suspendedConsoleError(clusterName, nodeName, statuses[0].Running)
	}
	if target.Phase.Stopped() {
		return fmt.Errorf("node %s is stopped — start the cluster first", nodeName)
	}

	dir, err := cluster.Dir(clusterName)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", filepath.Join(dir, nodeName+".console.sock"), 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to console: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if options.noFollow {
		// The dump is the whole point of --no-follow: stdout carries the
		// buffer and nothing else, so console evidence can be captured
		// without backgrounding the process and killing it on a timer (#410).
		_, _ = fmt.Fprintf(c.err, "%s/%s console ring buffer (kernel + machined logs; not following)\n", clusterName, nodeName)
		return dumpConsole(conn, c.out, options.lines, consoleDumpIdle, consoleDumpLimit)
	}

	_, _ = fmt.Fprintf(c.err, "attached to %s/%s console (kernel + machined logs; recent output replays) — detach with Ctrl-]\n", clusterName, nodeName)
	if target.Phase == daemon.PhaseConfigured {
		_, _ = fmt.Fprintln(c.err, configuredConsoleTip(target.IP, filepath.Join(dir, "talosconfig")))
	}

	stdinFd := int(os.Stdin.Fd())
	if term.IsTerminal(stdinFd) {
		oldState, err := term.MakeRaw(stdinFd)
		if err != nil {
			return fmt.Errorf("raw terminal: %w", err)
		}
		defer func() {
			_ = term.Restore(stdinFd, oldState)
			_, _ = fmt.Fprintln(c.err, "\ndetached")
		}()
	}

	done := make(chan error, 2)
	go func() { // guest -> terminal: its end always ends the session
		_, err := io.Copy(c.out, conn)
		done <- err
	}()
	// Note the deliberate asymmetry with the guest->terminal goroutine: this
	// one can stay parked in os.Stdin.Read after the session ends (stdin reads
	// are uninterruptible); process exit reclaims it.
	go func() { // terminal -> guest: only Ctrl-] ends the session — plain
		// stdin EOF (piped/non-tty use) just stops input forwarding
		_, err := io.Copy(conn, newDetachReader(os.Stdin))
		if errors.Is(err, errDetached) {
			done <- errDetached
		}
	}()
	err = <-done
	if errors.Is(err, errDetached) || err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

const (
	// consoleDumpIdle is how long a --no-follow dump waits for more bytes
	// before deciding the replay is over. The proxy replays its ring buffer in
	// one burst on attach, so a gap this long means the burst has ended and
	// anything further would be live output the dump is not there for.
	consoleDumpIdle = 300 * time.Millisecond
	// consoleDumpLimit bounds the dump even when the guest never falls silent:
	// a node spewing kernel output continuously must still let the command
	// return, which is the whole reason --no-follow exists (#410).
	consoleDumpLimit = 5 * time.Second
)

// deadlineReader is the console socket as the dump uses it: a stream whose
// reads can be bounded. net.Conn satisfies it, and so does net.Pipe, which is
// how the dump is tested without a VM.
type deadlineReader interface {
	io.Reader
	SetReadDeadline(time.Time) error
}

// dumpConsole writes the console ring buffer the proxy replays on attach and
// returns, keeping only the last lines when a bound was asked for.
func dumpConsole(source deadlineReader, out io.Writer, lines int, idle, limit time.Duration) error {
	limitAt := time.Now().Add(limit)
	var buffer bytes.Buffer
	chunk := make([]byte, 32*1024)
	for {
		wait := time.Now().Add(idle)
		if wait.After(limitAt) {
			wait = limitAt
		}
		if err := source.SetReadDeadline(wait); err != nil {
			return fmt.Errorf("bound the console read: %w", err)
		}
		n, err := source.Read(chunk)
		buffer.Write(chunk[:n])
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				break
			}
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("read console: %w", err)
		}
		if !time.Now().Before(limitAt) {
			break
		}
	}
	_, err := out.Write(tailLines(buffer.Bytes(), lines))
	return err
}

// tailLines keeps the last count lines of data; a count of zero keeps all of
// it. A trailing newline does not count as an empty last line.
func tailLines(data []byte, count int) []byte {
	if count <= 0 || len(data) == 0 {
		return data
	}
	end := len(bytes.TrimSuffix(data, []byte("\n")))
	found := 0
	for i := end - 1; i >= 0; i-- {
		if data[i] != '\n' {
			continue
		}
		found++
		if found == count {
			return data[i+1:]
		}
	}
	return data
}

// suspendedConsoleError says how to get a suspended node back, and the answer
// depends on the rest of the cluster: `tbx cluster resume` refuses outright
// once any sibling node is running, so with a live cluster the only command
// that revives this one node is `tbx node start` — which cold-boots it and
// drops its saved memory. Only a fully stopped cluster can be resumed.
func suspendedConsoleError(clusterName, nodeName string, clusterRunning bool) error {
	if clusterRunning {
		return fmt.Errorf("node %s is suspended while the rest of the cluster runs — boot it with: tbx node start %s %s (that cold-boots the node and discards its saved memory)", nodeName, shellquote.Quote(clusterName), shellquote.Quote(nodeName))
	}
	return fmt.Errorf("node %s is suspended — resume the cluster first: tbx cluster resume %s", nodeName, shellquote.Quote(clusterName))
}

func configuredConsoleTip(ip, talosconfig string) string {
	return fmt.Sprintf("tip: this node is configured — for the Talos dashboard TUI run: talosctl dashboard --talosconfig %s --nodes %[2]s --endpoints %[2]s", shellquote.Quote(talosconfig), ip)
}
