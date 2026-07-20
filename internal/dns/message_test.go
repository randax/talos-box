package dns

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

func TestAQueryAnswerRoundTrip(t *testing.T) {
	t.Parallel()

	const id = 0x1234
	query, err := encodeQuery("node.demo.k8s.test", id)
	if err != nil {
		t.Fatal(err)
	}
	q, err := parseQuestion(query)
	if err != nil {
		t.Fatal(err)
	}
	if q.name != "node.demo.k8s.test" || q.recordType != typeA || q.class != classIN {
		t.Fatalf("question = %#v", q)
	}
	response, err := answer(query, func(string) net.IP { return net.IPv4(172, 30, 0, 2) })
	if err != nil {
		t.Fatal(err)
	}
	ip, rcode, err := parseAnswerIP(response, id)
	if err != nil {
		t.Fatal(err)
	}
	if rcode != 0 || !ip.Equal(net.IPv4(172, 30, 0, 2)) {
		t.Fatalf("answer = %s rcode %d", ip, rcode)
	}
	if got := binary.BigEndian.Uint16(response[6:]); got != 1 {
		t.Fatalf("answer count = %d, want 1", got)
	}
}

func TestUnmatchedQueryReturnsNXDomain(t *testing.T) {
	t.Parallel()
	const id = 7
	query, err := encodeQuery("missing.k8s.test", id)
	if err != nil {
		t.Fatal(err)
	}
	response, err := answer(query, func(string) net.IP { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ip, rcode, err := parseAnswerIP(response, id)
	if err != nil {
		t.Fatal(err)
	}
	if ip != nil || rcode != 3 {
		t.Fatalf("answer = %v rcode %d, want NXDOMAIN", ip, rcode)
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()

	clusters := []cluster.Cluster{{
		Name: "demo", SubnetIndex: 7,
		Nodes: []cluster.Node{{Name: "demo-cp-1", MAC: "52:54:00:00:00:01"}},
	}}
	lease := func(mac string, subnetIndex int) string {
		if mac == "52:54:00:00:00:01" && subnetIndex == 7 {
			return "172.30.7.2"
		}
		return ""
	}
	tests := []struct {
		name string
		want net.IP
	}{
		{name: "demo-cp-1.demo.k8s.test", want: net.IPv4(172, 30, 7, 2)},
		{name: "app.demo.k8s.test", want: net.IPv4(172, 30, 7, 200)},
		{name: "nested.app.demo.k8s.test", want: net.IPv4(172, 30, 7, 200)},
		{name: "demo.k8s.test"},
		{name: "app.other.k8s.test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Resolve(tt.name, clusters, lease)
			if (got == nil) != (tt.want == nil) || got != nil && !got.Equal(tt.want) {
				t.Fatalf("Resolve(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestNodeWithoutLeaseDoesNotUseWildcard(t *testing.T) {
	t.Parallel()
	clusters := []cluster.Cluster{{Name: "demo", Nodes: []cluster.Node{{Name: "node"}}}}
	if got := Resolve("node.demo.k8s.test", clusters, func(string, int) string { return "" }); got != nil {
		t.Fatalf("Resolve() = %v, want nil", got)
	}
}
