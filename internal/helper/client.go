package helper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"golang.org/x/sys/unix"
)

// SocketPath is the helper's system-wide Unix socket.
const SocketPath = "/var/run/tbx-helper.sock"

// Client is a serialized connection to tbx-helper.
type Client struct {
	connection *net.UnixConn
	mu         sync.Mutex
}

// Connect connects to the system helper.
func Connect() (*Client, error) {
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: SocketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("connect to helper: %w", err)
	}
	return &Client{connection: connection}, nil
}

// Close closes the helper connection.
func (c *Client) Close() error {
	return c.connection.Close()
}

// Attach creates a vmnet interface and returns its datagram socket descriptor.
func (c *Client) Attach(cluster string, subnetIndex int, node string) (int, error) {
	args := struct {
		Cluster     string `json:"cluster"`
		SubnetIndex int    `json:"subnetIndex"`
		Node        string `json:"node"`
	}{Cluster: cluster, SubnetIndex: subnetIndex, Node: node}
	_, fd, err := c.call("net.attach", args, true)
	return fd, err
}

// Detach stops and removes a node's vmnet interface.
func (c *Client) Detach(cluster, node string) error {
	_, _, err := c.call("net.detach", map[string]string{"cluster": cluster, "node": node}, false)
	return err
}

// InstallDNS installs the k8s.test scoped resolver.
func (c *Client) InstallDNS(port int) error {
	_, _, err := c.call("dns.install", map[string]int{"port": port}, false)
	return err
}

// UninstallDNS removes the k8s.test scoped resolver.
func (c *Client) UninstallDNS() error {
	_, _, err := c.call("dns.uninstall", struct{}{}, false)
	return err
}

// EnableForwarding enables IPv4 forwarding on the host.
func (c *Client) EnableForwarding() error {
	_, _, err := c.call("forwarding.enable", struct{}{}, false)
	return err
}

// EnableBGP starts the host BGP speaker for a cluster.
func (c *Client) EnableBGP(cluster string, subnetIndex int, localASN, peerASN uint32) error {
	_, _, err := c.call("bgp.enable", map[string]any{
		"cluster": cluster, "subnetIndex": subnetIndex, "localASN": localASN, "peerASN": peerASN,
	}, false)
	return err
}

// DisableBGP stops a cluster's host BGP speaker.
func (c *Client) DisableBGP(cluster string) error {
	_, _, err := c.call("bgp.disable", map[string]any{"cluster": cluster}, false)
	return err
}

// Ping verifies that the helper is responsive.
func (c *Client) Ping() error {
	response, _, err := c.call("ping", struct{}{}, false)
	if err != nil {
		return err
	}
	var data struct {
		Pong bool `json:"pong"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return fmt.Errorf("decode ping response: %w", err)
	}
	if !data.Pong {
		return errors.New("helper did not return pong")
	}
	return nil
}

func (c *Client) call(op string, args any, wantFD bool) (Response, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rawArgs, err := json.Marshal(args)
	if err != nil {
		return Response{}, -1, fmt.Errorf("encode request arguments: %w", err)
	}
	wire, err := json.Marshal(Request{Op: op, Args: rawArgs})
	if err != nil {
		return Response{}, -1, fmt.Errorf("encode request: %w", err)
	}
	wire = append(wire, '\n')
	if err := writeAll(c.connection, wire); err != nil {
		return Response{}, -1, fmt.Errorf("write helper request: %w", err)
	}
	return receiveResponse(c.connection, wantFD)
}

func receiveResponse(connection *net.UnixConn, wantFD bool) (Response, int, error) {
	const maxResponseSize = 1 << 20
	var payload bytes.Buffer
	rights := make([]int, 0, 1)
	closeRights := func() {
		for _, fd := range rights {
			_ = unix.Close(fd)
		}
	}

	buffer := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(64*4))
	for !bytes.Contains(payload.Bytes(), []byte{'\n'}) {
		n, oobn, flags, _, err := connection.ReadMsgUnix(buffer, oob)
		if err != nil {
			closeRights()
			return Response{}, -1, fmt.Errorf("read helper response: %w", err)
		}
		if payload.Len()+n > maxResponseSize {
			closeRights()
			return Response{}, -1, errors.New("helper response is too large")
		}
		_, _ = payload.Write(buffer[:n])
		if oobn > 0 {
			messages, parseErr := unix.ParseSocketControlMessage(oob[:oobn])
			if parseErr != nil {
				closeRights()
				return Response{}, -1, fmt.Errorf("parse helper control message: %w", parseErr)
			}
			for _, message := range messages {
				fds, rightsErr := unix.ParseUnixRights(&message)
				if rightsErr != nil {
					closeRights()
					return Response{}, -1, fmt.Errorf("parse helper file descriptors: %w", rightsErr)
				}
				rights = append(rights, fds...)
			}
		}
		if flags&unix.MSG_CTRUNC != 0 {
			closeRights()
			return Response{}, -1, errors.New("helper response control data was truncated")
		}
		if n == 0 && oobn == 0 {
			closeRights()
			return Response{}, -1, io.ErrUnexpectedEOF
		}
	}

	line, _, _ := bytes.Cut(payload.Bytes(), []byte{'\n'})
	var response Response
	if err := json.Unmarshal(line, &response); err != nil {
		closeRights()
		return Response{}, -1, fmt.Errorf("decode helper response: %w", err)
	}
	if !response.OK {
		closeRights()
		if response.Error == "" {
			return response, -1, errors.New("helper operation failed")
		}
		return response, -1, errors.New(response.Error)
	}
	if wantFD {
		if len(rights) != 1 {
			closeRights()
			return response, -1, fmt.Errorf("helper returned %d file descriptors, want 1", len(rights))
		}
		return response, rights[0], nil
	}
	if len(rights) != 0 {
		closeRights()
		return response, -1, fmt.Errorf("helper unexpectedly returned %d file descriptors", len(rights))
	}
	return response, -1, nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
