package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/term"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
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

func (c cli) runConsole(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: tbx console <cluster> <node>")
	}
	clusterName, nodeName := args[0], args[1]

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
	if target.Phase == daemon.PhaseStopped {
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

	_, _ = fmt.Fprintf(c.err, "attached to %s/%s console (kernel + machined logs; recent output replays) — detach with Ctrl-]\n", clusterName, nodeName)
	if target.Phase == daemon.PhaseConfigured {
		_, _ = fmt.Fprintf(c.err, "tip: this node is configured — for the Talos dashboard TUI run: talosctl dashboard --nodes %s\n", target.IP)
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
