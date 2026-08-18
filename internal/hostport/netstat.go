// Package hostport reads the host's listening TCP sockets out of netstat
// output. It exists for macOS, where an unprivileged `lsof -iTCP:<port>` cannot
// see a root-owned socket at all: netstat reports every listener without
// privilege, at the cost of naming no owner (#359).
package hostport

import (
	"bufio"
	"strconv"
	"strings"
)

// Listener is one listening TCP socket as the host reported it.
type Listener struct {
	// Address is the local address the socket is bound to, without the port.
	// A wildcard bind reports it verbatim ("*", "0.0.0.0" or "::").
	Address string
	// Line is the raw netstat line, so a finding can quote what it saw rather
	// than a reconstruction of it.
	Line string
}

// ParseNetstatListeners returns the TCP sockets listening on port in BSD
// `netstat -an` output. Lines it cannot read are skipped: netstat's header and
// its unix-socket section are part of the same stream.
func ParseNetstatListeners(output []byte, port int) []Listener {
	var listeners []Listener
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		// proto recv-q send-q local-address foreign-address state
		if len(fields) < 6 || !strings.HasPrefix(fields[0], "tcp") {
			continue
		}
		if fields[len(fields)-1] != "LISTEN" {
			continue
		}
		address, listening := splitNetstatEndpoint(fields[3], port)
		if !listening {
			continue
		}
		listeners = append(listeners, Listener{Address: address, Line: line})
	}
	return listeners
}

// Wildcard reports whether addr is an any-address bind. Such a listener sits in
// front of every local address on the port at once, including the cluster
// gateway the host BGP speaker needs.
func Wildcard(addr string) bool {
	switch addr {
	case "*", "0.0.0.0", "::":
		return true
	default:
		return false
	}
}

// splitNetstatEndpoint splits a BSD endpoint — "*.179", "172.30.0.1.179",
// "::.179" — into its address and reports whether it names port.
func splitNetstatEndpoint(endpoint string, port int) (string, bool) {
	separator := strings.LastIndex(endpoint, ".")
	if separator < 0 {
		return "", false
	}
	value, err := strconv.Atoi(endpoint[separator+1:])
	if err != nil || value != port {
		return "", false
	}
	return endpoint[:separator], true
}
