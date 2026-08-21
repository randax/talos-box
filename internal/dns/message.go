// Package dns implements the small authoritative resolver used by talosbox.
package dns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
)

const (
	typeA   = 1
	typeSOA = 6
	classIN = 1

	// negativeTTL bounds how long a client may cache a miss in a zone we own.
	// macOS derives its negative-cache lifetime from the SOA minimum and falls
	// back to ~30s when the authority section is empty, which strands a node
	// queried just before its DHCP lease lands.
	negativeTTL = 5

	// flagRecursionAvailable is the RA header bit. We are authoritative for our
	// own zones and never recurse for them, but guests reach us through the
	// cluster's CoreDNS, which forwards and then reports ";; Got recursion not
	// available" on every response that leaves RA clear — noise that reads as a
	// DNS failure. Setting RA is honest for the path as a whole: names outside
	// our zones are in fact resolved recursively by the upstream we forward to.
	flagRecursionAvailable = 0x0080
)

type question struct {
	name       string
	recordType uint16
	class      uint16
	end        int
}

func encodeName(name string) ([]byte, error) {
	encoded := make([]byte, 0, len(name)+2)
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("invalid DNS label %q", label)
		}
		encoded = append(encoded, byte(len(label)))
		encoded = append(encoded, label...)
	}
	return append(encoded, 0), nil
}

func encodeQuery(name string, id uint16) ([]byte, error) {
	message := make([]byte, 12)
	binary.BigEndian.PutUint16(message, id)
	binary.BigEndian.PutUint16(message[2:], 0x0100)
	binary.BigEndian.PutUint16(message[4:], 1)
	encoded, err := encodeName(name)
	if err != nil {
		return nil, err
	}
	message = append(message, encoded...)
	message = append(message, 0, typeA, 0, classIN)
	return message, nil
}

func parseQuestion(message []byte) (question, error) {
	if len(message) < 12 || binary.BigEndian.Uint16(message[4:]) != 1 {
		return question{}, errors.New("DNS message must contain one question")
	}
	offset := 12
	labels := make([]string, 0, 4)
	for {
		if offset >= len(message) {
			return question{}, errors.New("truncated DNS name")
		}
		length := int(message[offset])
		offset++
		if length == 0 {
			break
		}
		if length > 63 || offset+length > len(message) {
			return question{}, errors.New("invalid DNS label")
		}
		labels = append(labels, string(message[offset:offset+length]))
		offset += length
	}
	if offset+4 > len(message) {
		return question{}, errors.New("truncated DNS question")
	}
	return question{
		name:       strings.Join(labels, "."),
		recordType: binary.BigEndian.Uint16(message[offset:]),
		class:      binary.BigEndian.Uint16(message[offset+2:]),
		end:        offset + 4,
	}, nil
}

// authoritySOA renders the AUTHORITY-section SOA published with a miss in one
// of our zones. Only the minimum field matters to clients — it caps negative
// caching — so the remaining timers are fixed, deterministic placeholders. The
// zone is unencodable only if it came from a name we could not have parsed, in
// which case the caller simply omits the authority section.
func authoritySOA(zone string) []byte {
	owner, err := encodeName(zone)
	if err != nil {
		return nil
	}
	mailbox, err := encodeName("hostmaster." + zone)
	if err != nil {
		return nil
	}
	rdata := append(append([]byte(nil), owner...), mailbox...)
	rdata = binary.BigEndian.AppendUint32(rdata, 1)     // serial
	rdata = binary.BigEndian.AppendUint32(rdata, 3600)  // refresh
	rdata = binary.BigEndian.AppendUint32(rdata, 600)   // retry
	rdata = binary.BigEndian.AppendUint32(rdata, 86400) // expire
	rdata = binary.BigEndian.AppendUint32(rdata, negativeTTL)

	record := append([]byte(nil), owner...)
	record = binary.BigEndian.AppendUint16(record, typeSOA)
	record = binary.BigEndian.AppendUint16(record, classIN)
	record = binary.BigEndian.AppendUint32(record, negativeTTL)
	record = binary.BigEndian.AppendUint16(record, uint16(len(rdata)))
	return append(record, rdata...)
}

// answer resolves a query locally. A non-empty zone names the apex we own,
// whose SOA is attached to a miss so clients bound their negative caching. A
// miss is either NXDOMAIN (the name is unknown) or NODATA (the name exists but
// not with the queried type/class); both carry the SOA, only the first sets
// rcode 3.
func answer(query []byte, lookup func(string) net.IP, zone string) ([]byte, error) {
	q, err := parseQuestion(query)
	if err != nil {
		return nil, err
	}
	ip := lookup(q.name).To4()
	matched := q.recordType == typeA && q.class == classIN && ip != nil
	// RFC 2308 separates the two misses, and with an SOA attached the
	// difference is no longer cosmetic: NXDOMAIN denies the whole name, so an
	// AAAA lookup for a live node would plant a cacheable denial that makes its
	// A record unresolvable for the negative-TTL window. A name we do hold an
	// address for answers NODATA — rcode 0, no answers, same SOA — and rcode 3
	// is reserved for names we know nothing about.
	nameExists := ip != nil
	response := append([]byte(nil), query[:q.end]...)
	flags := uint16(0x8400) | flagRecursionAvailable | binary.BigEndian.Uint16(query[2:])&0x0100
	if !matched && !nameExists {
		flags |= 3
	}
	binary.BigEndian.PutUint16(response[2:], flags)
	binary.BigEndian.PutUint16(response[4:], 1)
	binary.BigEndian.PutUint16(response[6:], 0)
	binary.BigEndian.PutUint16(response[8:], 0)
	binary.BigEndian.PutUint16(response[10:], 0)
	if !matched {
		if zone == "" {
			return response, nil
		}
		soa := authoritySOA(zone)
		if soa == nil {
			return response, nil
		}
		binary.BigEndian.PutUint16(response[8:], 1)
		return append(response, soa...), nil
	}
	binary.BigEndian.PutUint16(response[6:], 1)
	response = append(response,
		0xc0, 0x0c,
		0, typeA,
		0, classIN,
		0, 0, 0, 30,
		0, 4,
	)
	response = append(response, ip...)
	return response, nil
}

func errorAnswer(query []byte, rcode uint16) ([]byte, error) {
	q, err := parseQuestion(query)
	if err != nil {
		return nil, err
	}
	response := append([]byte(nil), query[:q.end]...)
	flags := uint16(0x8000) | flagRecursionAvailable | binary.BigEndian.Uint16(query[2:])&0x0100 | rcode&0xf
	binary.BigEndian.PutUint16(response[2:], flags)
	binary.BigEndian.PutUint16(response[4:], 1)
	binary.BigEndian.PutUint16(response[6:], 0)
	binary.BigEndian.PutUint16(response[8:], 0)
	binary.BigEndian.PutUint16(response[10:], 0)
	return response, nil
}

func parseAnswerIP(message []byte, id uint16) (net.IP, int, error) {
	if len(message) < 12 || binary.BigEndian.Uint16(message) != id || binary.BigEndian.Uint16(message[2:])&0x8000 == 0 {
		return nil, 0, errors.New("invalid DNS response")
	}
	rcode := int(binary.BigEndian.Uint16(message[2:]) & 0xf)
	q, err := parseQuestion(message)
	if err != nil {
		return nil, 0, err
	}
	if binary.BigEndian.Uint16(message[6:]) == 0 {
		return nil, rcode, nil
	}
	offset := q.end
	if offset+16 > len(message) || message[offset] != 0xc0 ||
		binary.BigEndian.Uint16(message[offset+2:]) != typeA ||
		binary.BigEndian.Uint16(message[offset+10:]) != 4 {
		return nil, 0, errors.New("invalid DNS A answer")
	}
	return net.IPv4(message[offset+12], message[offset+13], message[offset+14], message[offset+15]), rcode, nil
}
