package hypervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const maxQMPFrame = 1 << 20

type qmpClient struct {
	conn   net.Conn
	reader *bufio.Reader

	mu     sync.Mutex
	nextID uint64

	closeOnce sync.Once
	closeErr  error
}

type qmpRequest struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
	ID        uint64 `json:"id"`
}

type qmpMessage struct {
	Greeting json.RawMessage `json:"QMP,omitempty"`
	Event    string          `json:"event,omitempty"`
	Return   json.RawMessage `json:"return,omitempty"`
	Error    *qmpError       `json:"error,omitempty"`
	ID       *uint64         `json:"id,omitempty"`
}

type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *qmpError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Desc == "" {
		return "qmp error: " + e.Class
	}
	if e.Class == "" {
		return "qmp error: " + e.Desc
	}
	return fmt.Sprintf("qmp error %s: %s", e.Class, e.Desc)
}

func (e *qmpError) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.Class == "DeviceNotActive" {
		return ErrDeviceNotActive
	}
	return nil
}

func newQMPClient(ctx context.Context, conn net.Conn) (*qmpClient, error) {
	client := &qmpClient{
		conn:   conn,
		reader: bufio.NewReaderSize(conn, maxQMPFrame+1),
	}
	if err := client.handshake(ctx); err != nil {
		_ = client.close()
		return nil, err
	}
	return client, nil
}

func (c *qmpClient) handshake(ctx context.Context) error {
	if err := c.readGreeting(ctx); err != nil {
		return err
	}
	return c.execute(ctx, "qmp_capabilities", nil, nil)
}

func (c *qmpClient) readGreeting(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	stop, err := armConnContext(ctx, c.conn)
	if err != nil {
		return err
	}
	defer stop()

	for {
		message, err := c.readMessage()
		if err != nil {
			return mapQMPContextError(ctx, fmt.Errorf("read qmp greeting: %w", err))
		}
		switch {
		case len(message.Greeting) != 0:
			return nil
		case message.Event != "":
			continue
		default:
			return fmt.Errorf("read qmp greeting: unexpected message")
		}
	}
}

func (c *qmpClient) execute(ctx context.Context, command string, arguments any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID + 1
	c.nextID = id

	stop, err := armConnContext(ctx, c.conn)
	if err != nil {
		return err
	}
	defer stop()

	request := qmpRequest{
		Execute:   command,
		Arguments: arguments,
		ID:        id,
	}
	if err := c.writeMessage(request); err != nil {
		return mapQMPContextError(ctx, fmt.Errorf("send qmp command %q: %w", command, err))
	}

	for {
		message, err := c.readMessage()
		if err != nil {
			return mapQMPContextError(ctx, fmt.Errorf("read qmp response for %q: %w", command, err))
		}
		switch {
		case message.Event != "":
			continue
		case message.ID == nil:
			if len(message.Greeting) != 0 {
				continue
			}
			return fmt.Errorf("read qmp response for %q: missing id", command)
		case *message.ID < id:
			continue
		case *message.ID > id:
			return fmt.Errorf("read qmp response for %q: unexpected id %d", command, *message.ID)
		}

		if message.Error != nil {
			return fmt.Errorf("qmp command %q failed: %w", command, message.Error)
		}
		if result != nil && len(message.Return) != 0 && string(message.Return) != "null" {
			if err := json.Unmarshal(message.Return, result); err != nil {
				return fmt.Errorf("decode qmp response for %q: %w", command, err)
			}
		}
		return nil
	}
}

func (c *qmpClient) close() error {
	c.closeOnce.Do(func() {
		if c.conn != nil {
			c.closeErr = c.conn.Close()
		}
	})
	return c.closeErr
}

func (c *qmpClient) writeMessage(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	for len(data) > 0 {
		n, err := c.conn.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		data = data[n:]
	}
	return nil
}

func (c *qmpClient) readMessage() (qmpMessage, error) {
	var message qmpMessage
	line, err := c.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return message, fmt.Errorf("qmp frame exceeds %d bytes", maxQMPFrame)
	}
	if len(line) > maxQMPFrame {
		return message, fmt.Errorf("qmp frame exceeds %d bytes", maxQMPFrame)
	}
	if err != nil {
		if len(line) == 0 {
			return message, err
		}
		if !errors.Is(err, io.EOF) {
			return message, err
		}
	}
	if err := json.Unmarshal(line, &message); err != nil {
		return message, err
	}
	return message, err
}

func armConnContext(ctx context.Context, conn net.Conn) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear qmp deadline: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set qmp deadline: %w", err)
		}
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	return func() {
		close(done)
		<-stopped
		_ = conn.SetDeadline(time.Time{})
	}, nil
}

func mapQMPContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}
	return err
}
