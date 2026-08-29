//go:build darwin

package cluster

import (
	"net"
	"testing"
)

func TestDarwinRouteLooksLikeTunnel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		interfaceName string
		want          bool
	}{
		{name: "first tunnel", interfaceName: "utun0", want: true},
		{name: "later tunnel", interfaceName: "utun42", want: true},
		{name: "ethernet", interfaceName: "en0", want: false},
		{name: "bridge", interfaceName: "bridge100", want: false},
		{name: "empty", interfaceName: "", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := darwinRouteLooksLikeTunnel(test.interfaceName); got != test.want {
				t.Errorf("darwinRouteLooksLikeTunnel(%q) = %t, want %t", test.interfaceName, got, test.want)
			}
		})
	}
}

func TestParseDarwinHostRouteCarriesTunnelSignal(t *testing.T) {
	t.Parallel()

	output := []byte("route to: 172.30.0.2\ndestination: default\ngateway: 10.0.0.1\ninterface: utun9\n")
	got, err := parseDarwinHostRoute(output, net.ParseIP("172.30.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.LooksLikeTunnel {
		t.Fatalf("parsed route = %+v, want tunnel signal", got)
	}
}
