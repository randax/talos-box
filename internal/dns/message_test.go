package dns

import (
	"encoding/binary"
	"net"
	"strings"
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
	response, err := answer(query, func(string) net.IP { return net.IPv4(172, 30, 0, 2) }, "")
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
	response, err := answer(query, func(string) net.IP { return nil }, "")
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
	if got := binary.BigEndian.Uint16(response[8:]); got != 0 {
		t.Fatalf("authority count = %d, want 0 without an owned zone", got)
	}
}

// soaRecord is the decoded AUTHORITY-section SOA of a response.
type soaRecord struct {
	owner   string
	ttl     uint32
	mname   string
	rname   string
	minimum uint32
}

func parseAuthoritySOA(t *testing.T, message []byte) soaRecord {
	t.Helper()
	if got := binary.BigEndian.Uint16(message[8:]); got != 1 {
		t.Fatalf("authority count = %d, want 1", got)
	}
	q, err := parseQuestion(message)
	if err != nil {
		t.Fatal(err)
	}
	offset := q.end
	if answers := binary.BigEndian.Uint16(message[6:]); answers != 0 {
		t.Fatalf("answer count = %d, want 0", answers)
	}
	var record soaRecord
	record.owner, offset = decodeName(t, message, offset)
	if recordType := binary.BigEndian.Uint16(message[offset:]); recordType != typeSOA {
		t.Fatalf("authority record type = %d, want SOA", recordType)
	}
	if class := binary.BigEndian.Uint16(message[offset+2:]); class != classIN {
		t.Fatalf("authority record class = %d, want IN", class)
	}
	record.ttl = binary.BigEndian.Uint32(message[offset+4:])
	rdlength := int(binary.BigEndian.Uint16(message[offset+8:]))
	offset += 10
	if offset+rdlength != len(message) {
		t.Fatalf("SOA rdlength = %d, covers %d bytes", rdlength, len(message)-offset)
	}
	record.mname, offset = decodeName(t, message, offset)
	record.rname, offset = decodeName(t, message, offset)
	record.minimum = binary.BigEndian.Uint32(message[offset+16:])
	return record
}

func decodeName(t *testing.T, message []byte, offset int) (string, int) {
	t.Helper()
	labels := make([]string, 0, 4)
	for {
		if offset >= len(message) {
			t.Fatal("truncated name in response")
		}
		length := int(message[offset])
		offset++
		if length == 0 {
			return strings.Join(labels, "."), offset
		}
		if length > 63 || offset+length > len(message) {
			t.Fatalf("invalid label length %d", length)
		}
		labels = append(labels, string(message[offset:offset+length]))
		offset += length
	}
}

func TestNXDomainCarriesSOAThatBoundsNegativeCaching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		zone string
	}{
		{name: "missing.demo.k8s.test", zone: "k8s.test"},
		{name: "missing.lab.internal", zone: "lab.internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query, err := encodeQuery(tt.name, 11)
			if err != nil {
				t.Fatal(err)
			}
			response, err := answer(query, func(string) net.IP { return nil }, tt.zone)
			if err != nil {
				t.Fatal(err)
			}
			if _, rcode, err := parseAnswerIP(response, 11); err != nil || rcode != 3 {
				t.Fatalf("parseAnswerIP() = rcode %d, err %v, want NXDOMAIN", rcode, err)
			}
			soa := parseAuthoritySOA(t, response)
			// The literal 5 is the point of the record: macOS caches the miss
			// for the SOA minimum, so a node still awaiting its lease resolves
			// seconds later instead of after ~30s.
			want := soaRecord{
				owner:   tt.zone,
				ttl:     5,
				mname:   tt.zone,
				rname:   "hostmaster." + tt.zone,
				minimum: 5,
			}
			if soa != want {
				t.Fatalf("SOA = %+v, want %+v", soa, want)
			}
		})
	}
}

// TestExistingNameWithWrongTypeIsNODATANotNXDomain is the sibling of the
// NXDOMAIN case: the SOA makes a negative answer cacheable, so denying the
// whole name because the question was AAAA would strand the node's A record for
// the negative-TTL window. RFC 2308 calls for NODATA — rcode 0, no answers, the
// same authority SOA.
func TestExistingNameWithWrongTypeIsNODATANotNXDomain(t *testing.T) {
	t.Parallel()

	const typeAAAA = 28
	query, err := encodeQuery("node.demo.k8s.test", 17)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(query[len(query)-4:], typeAAAA)

	response, err := answer(query, func(string) net.IP { return net.IPv4(172, 30, 7, 2) }, "k8s.test")
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint16(response[2:]) & 0xf; got != 0 {
		t.Fatalf("rcode = %d, want 0 (NODATA) for a name whose A record exists", got)
	}
	if got := binary.BigEndian.Uint16(response[6:]); got != 0 {
		t.Fatalf("answer count = %d, want 0", got)
	}
	if got := binary.BigEndian.Uint16(response[8:]); got != 1 {
		t.Fatalf("authority count = %d, want the SOA that bounds negative caching", got)
	}
	if soa := parseAuthoritySOA(t, response); soa.owner != "k8s.test" || soa.minimum != negativeTTL {
		t.Fatalf("SOA = %+v, want k8s.test with minimum %d", soa, negativeTTL)
	}
}

func TestMatchedAnswerHasNoAuthoritySection(t *testing.T) {
	t.Parallel()

	query, err := encodeQuery("node.demo.k8s.test", 13)
	if err != nil {
		t.Fatal(err)
	}
	response, err := answer(query, func(string) net.IP { return net.IPv4(172, 30, 7, 2) }, "k8s.test")
	if err != nil {
		t.Fatal(err)
	}
	ip, rcode, err := parseAnswerIP(response, 13)
	if err != nil {
		t.Fatal(err)
	}
	if rcode != 0 || !ip.Equal(net.IPv4(172, 30, 7, 2)) {
		t.Fatalf("answer = %v rcode %d", ip, rcode)
	}
	if got := binary.BigEndian.Uint16(response[8:]); got != 0 {
		t.Fatalf("authority count = %d, want 0", got)
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

func TestResolveCustomAndNestedDomains(t *testing.T) {
	t.Parallel()

	clusters := []cluster.Cluster{
		{
			Name: "outer", SubnetIndex: 3, Domain: "lab.test",
			Nodes: []cluster.Node{{Name: "outer-cp-1", MAC: "52:54:00:00:00:03"}},
		},
		{
			Name: "inner", SubnetIndex: 9, Domain: "staging.lab.test",
			Nodes: []cluster.Node{{Name: "inner-cp-1", MAC: "52:54:00:00:00:09"}},
		},
	}
	lease := func(mac string, subnetIndex int) string {
		switch mac {
		case "52:54:00:00:00:03":
			return "172.30.3.2"
		case "52:54:00:00:00:09":
			return "172.30.9.2"
		}
		return ""
	}
	tests := []struct {
		name string
		want net.IP
	}{
		{name: "outer-cp-1.lab.test", want: net.IPv4(172, 30, 3, 2)},
		{name: "inner-cp-1.staging.lab.test", want: net.IPv4(172, 30, 9, 2)},
		{name: "app.lab.test", want: net.IPv4(172, 30, 3, 200)},
		// longest suffix wins: never falls through to the enclosing cluster
		{name: "app.staging.lab.test", want: net.IPv4(172, 30, 9, 200)},
		{name: "deep.app.staging.lab.test", want: net.IPv4(172, 30, 9, 200)},
		{name: "lab.test"},
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

func TestNodeRecordNeverCrossesIntoNestedDomain(t *testing.T) {
	t.Parallel()

	// The outer cluster has a node whose FQDN equals the inner cluster's
	// domain apex. Longest-suffix ownership means the inner cluster owns the
	// name, and its apex has no record.
	clusters := []cluster.Cluster{
		{
			Name: "outer", SubnetIndex: 3, Domain: "lab.test",
			Nodes: []cluster.Node{{Name: "app", MAC: "52:54:00:00:00:03"}},
		},
		{Name: "inner", SubnetIndex: 9, Domain: "app.lab.test"},
	}
	lease := func(string, int) string { return "172.30.3.2" }
	if got := Resolve("app.lab.test", clusters, lease); got != nil {
		t.Fatalf("Resolve(app.lab.test) = %v, want nil (inner apex owns the name)", got)
	}
}

func TestNodeWithoutLeaseDoesNotUseWildcard(t *testing.T) {
	t.Parallel()
	clusters := []cluster.Cluster{{Name: "demo", Nodes: []cluster.Node{{Name: "node"}}}}
	if got := Resolve("node.demo.k8s.test", clusters, func(string, int) string { return "" }); got != nil {
		t.Fatalf("Resolve() = %v, want nil", got)
	}
}
