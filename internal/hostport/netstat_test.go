package hostport

import "testing"

// Real `netstat -an` output on macOS: a stray listener on every address, the
// host BGP speaker on one cluster gateway, an established session on the same
// port, an unrelated port, and the header lines that share the stream.
const darwinNetstat = `Active Internet connections (including servers)
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)
tcp4       0      0  *.179                  *.*                    LISTEN
tcp46      0      0  *.179                  *.*                    LISTEN
tcp4       0      0  172.30.0.1.179         *.*                    LISTEN
tcp4       0      0  172.30.0.1.179         172.30.0.2.51314       ESTABLISHED
tcp6       0      0  ::.179                 *.*                    LISTEN
tcp4       0      0  127.0.0.1.5399         *.*                    LISTEN
Active LOCAL (UNIX) domain sockets
f1e2d3c4 stream      0      0        0 f1e2d3c5        0        0 /tmp/tbxd.sock
`

func TestParseNetstatListenersFindsOnlyTheListeningSocketsOnThePort(t *testing.T) {
	listeners := ParseNetstatListeners([]byte(darwinNetstat), 179)

	var addresses []string
	for _, listener := range listeners {
		if listener.Line == "" {
			t.Fatalf("listener %+v carries no raw line to quote", listener)
		}
		addresses = append(addresses, listener.Address)
	}
	want := []string{"*", "*", "172.30.0.1", "::"}
	if len(addresses) != len(want) {
		t.Fatalf("listeners = %q, want %q", addresses, want)
	}
	for i, address := range addresses {
		if address != want[i] {
			t.Fatalf("listeners = %q, want %q", addresses, want)
		}
	}
}

// A port whose digits are a prefix of another's must not match it: 179 and
// 17900 are different sockets.
func TestParseNetstatListenersMatchesTheWholePort(t *testing.T) {
	const output = `tcp4       0      0  *.17900                *.*                    LISTEN
`
	if listeners := ParseNetstatListeners([]byte(output), 179); len(listeners) != 0 {
		t.Fatalf("listeners = %+v, want none for a different port", listeners)
	}
}

func TestWildcardRecognisesAnyAddressBinds(t *testing.T) {
	for _, address := range []string{"*", "0.0.0.0", "::"} {
		if !Wildcard(address) {
			t.Fatalf("Wildcard(%q) = false, want true", address)
		}
	}
	for _, address := range []string{"172.30.0.1", "127.0.0.1", "fe80::1%en0"} {
		if Wildcard(address) {
			t.Fatalf("Wildcard(%q) = true, want false", address)
		}
	}
}
