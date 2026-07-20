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
	classIN = 1
)

type question struct {
	name       string
	recordType uint16
	class      uint16
	end        int
}

func encodeQuery(name string, id uint16) ([]byte, error) {
	message := make([]byte, 12)
	binary.BigEndian.PutUint16(message, id)
	binary.BigEndian.PutUint16(message[2:], 0x0100)
	binary.BigEndian.PutUint16(message[4:], 1)
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("invalid DNS label %q", label)
		}
		message = append(message, byte(len(label)))
		message = append(message, label...)
	}
	message = append(message, 0, 0, typeA, 0, classIN)
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

func answer(query []byte, lookup func(string) net.IP) ([]byte, error) {
	q, err := parseQuestion(query)
	if err != nil {
		return nil, err
	}
	ip := lookup(q.name).To4()
	matched := q.recordType == typeA && q.class == classIN && ip != nil
	response := append([]byte(nil), query[:q.end]...)
	flags := uint16(0x8400) | binary.BigEndian.Uint16(query[2:])&0x0100
	if !matched {
		flags |= 3
	}
	binary.BigEndian.PutUint16(response[2:], flags)
	binary.BigEndian.PutUint16(response[4:], 1)
	binary.BigEndian.PutUint16(response[6:], 0)
	binary.BigEndian.PutUint16(response[8:], 0)
	binary.BigEndian.PutUint16(response[10:], 0)
	if !matched {
		return response, nil
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
