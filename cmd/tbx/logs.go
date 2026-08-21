package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const logsUsage = "usage: tbx logs [cluster] [--cluster name] [--follow] [--lines n]"

// defaultLogLines is the tail a bare `tbx logs` prints: enough to cover one
// operation's narration without paging the whole append-only log.
const defaultLogLines = 200

// logFollowInterval is how often --follow looks for appended bytes. It is a var
// only so tests can shorten it.
var logFollowInterval = 500 * time.Millisecond

// runLogs prints the daemon log the runbooks route diagnosis through. Reading
// it used to require knowing ~/.talosbox/tbxd.log out of band (#402).
func (c cli) runLogs(args []string) error {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(c.err)
	follow := flags.Bool("follow", false, "keep printing lines as the daemon writes them")
	lines := flags.Int("lines", defaultLogLines, "print this many trailing lines (0 for the whole log)")
	clusterFlag := flags.String("cluster", "", "print only lines about this cluster")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) > 1 || (len(positionals) == 1 && *clusterFlag != "") {
		return errors.New(logsUsage)
	}
	if *lines < 0 {
		return errors.New("--lines must not be negative")
	}
	name := *clusterFlag
	if len(positionals) == 1 {
		name = positionals[0]
	}

	path, err := daemonLogPath()
	if err != nil {
		return err
	}
	offset, err := printLogTail(c.out, path, *lines, name)
	if err != nil {
		return err
	}
	if !*follow {
		return nil
	}
	return followLog(c.out, path, offset, name, nil)
}

// printLogTail prints the last count matching lines and reports the offset the
// tail ended at, so a follow resumes exactly where it stopped.
func printLogTail(w io.Writer, path string, count int, cluster string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("no daemon log at %s yet; run a daemon-backed command such as `tbx status` to start tbxd, then retry", path)
		}
		return 0, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()

	var tail []string
	scanner := bufio.NewScanner(io.LimitReader(file, size))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !logLineMatches(line, cluster) {
			continue
		}
		tail = append(tail, line)
		if count > 0 && len(tail) > count {
			tail = tail[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	for _, line := range tail {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return 0, err
		}
	}
	return size, nil
}

// followLog prints lines appended after offset until stop is closed. A nil stop
// follows until the process is interrupted, the way `tail -f` does.
func followLog(w io.Writer, path string, offset int64, cluster string, stop <-chan struct{}) error {
	pending := ""
	for {
		select {
		case <-stop:
			return nil
		case <-time.After(logFollowInterval):
		}
		grown, appended, err := readAppended(path, offset)
		if err != nil {
			return err
		}
		offset = grown
		pending += appended
		for {
			index := strings.IndexByte(pending, '\n')
			if index < 0 {
				break
			}
			line := pending[:index]
			pending = pending[index+1:]
			if !logLineMatches(line, cluster) {
				continue
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}
}

// readAppended reads whatever was written past offset. A file that shrank was
// rotated or replaced, so following restarts from its beginning.
func readAppended(path string, offset int64) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return offset, "", nil // the daemon may be restarting
		}
		return offset, "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return offset, "", err
	}
	if info.Size() < offset {
		offset = 0
	}
	if info.Size() == offset {
		return offset, "", nil
	}
	content := make([]byte, info.Size()-offset)
	read, err := file.ReadAt(content, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return offset, "", err
	}
	return offset + int64(read), string(content[:read]), nil
}

// logLineMatches applies the cluster filter. The daemon narrates per-cluster
// work under two conventions: node-scoped lines carry a "<cluster>/<node>"
// subject — `balloon qa-cil/qa-cil-cp-1: …` — while cluster-scoped ones name
// the cluster alone — `provision qa-cil: waiting on …`. Matching only the
// slash form hid every gate, status and provision line the daemon writes, so
// the filter accepts both (#402).
func logLineMatches(line, cluster string) bool {
	if cluster == "" {
		return true
	}
	return strings.Contains(line, cluster+"/") ||
		strings.Contains(line, cluster+":") ||
		strings.Contains(line, cluster+" ")
}
