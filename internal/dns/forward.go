package dns

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const upstreamTimeout = 2 * time.Second

// SystemForward forwards a DNS packet through the name servers selected by
// the host resolver configuration. On systemd-resolved hosts this targets the
// 127.0.0.53 stub, preserving its split-DNS routing and cache; on other hosts
// it follows the configured upstream servers. The configuration is read for
// every query so VPN and network changes take effect without restarting tbxd.
func SystemForward(query []byte) ([]byte, error) {
	content, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil, fmt.Errorf("read host resolver configuration: %w", err)
	}
	servers := resolverNameServers(content)
	if len(servers) == 0 {
		return nil, errors.New("host resolver configuration has no name servers")
	}

	var result error
	for _, server := range servers {
		response, err := exchangeDNS(query, net.JoinHostPort(server, "53"))
		if err == nil {
			return response, nil
		}
		result = errors.Join(result, fmt.Errorf("forward DNS to %s: %w", server, err))
	}
	return nil, result
}

func exchangeDNS(query []byte, address string) ([]byte, error) {
	response, err := exchangeUDP(query, address)
	if err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint16(response[2:])&0x0200 == 0 {
		return response, nil
	}
	return exchangeTCP(query, address)
}

func resolverNameServers(content []byte) []string {
	var servers []string
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[0] != "nameserver" || net.ParseIP(fields[1]) == nil {
			continue
		}
		servers = append(servers, fields[1])
	}
	return servers
}

func exchangeUDP(query []byte, address string) ([]byte, error) {
	connection, err := net.DialTimeout("udp", address, upstreamTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(upstreamTimeout)); err != nil {
		return nil, err
	}
	if _, err := connection.Write(query); err != nil {
		return nil, err
	}
	response := make([]byte, 65535)
	size, err := connection.Read(response)
	if err != nil {
		return nil, err
	}
	return validateForwardedResponse(query, response[:size])
}

func exchangeTCP(query []byte, address string) ([]byte, error) {
	if len(query) > int(^uint16(0)) {
		return nil, errors.New("DNS query is too large for TCP framing")
	}
	connection, err := net.DialTimeout("tcp", address, upstreamTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(upstreamTimeout)); err != nil {
		return nil, err
	}
	frame := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(frame, uint16(len(query)))
	copy(frame[2:], query)
	if err := writeFull(connection, frame); err != nil {
		return nil, err
	}
	length := make([]byte, 2)
	if _, err := io.ReadFull(connection, length); err != nil {
		return nil, err
	}
	response := make([]byte, int(binary.BigEndian.Uint16(length)))
	if _, err := io.ReadFull(connection, response); err != nil {
		return nil, err
	}
	return validateForwardedResponse(query, response)
}

func validateForwardedResponse(query, response []byte) ([]byte, error) {
	if len(query) < 2 || len(response) < 12 ||
		binary.BigEndian.Uint16(response) != binary.BigEndian.Uint16(query) ||
		binary.BigEndian.Uint16(response[2:])&0x8000 == 0 {
		return nil, errors.New("upstream returned an invalid DNS response")
	}
	return response, nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
