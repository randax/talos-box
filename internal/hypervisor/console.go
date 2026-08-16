package hypervisor

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	consoleWriteTimeout = 2 * time.Second
	consoleScrollback   = 64 * 1024
)

type consoleProxy struct {
	listener *net.UnixListener
	input    io.Writer
	output   io.Reader
	closeIO  func() error
	ring     *ringBuffer

	mu     sync.Mutex
	client net.Conn

	// writeMu keeps attach replay strictly before subsequent live output.
	writeMu sync.Mutex
}

func newConsoleProxy(path string) (*consoleProxy, *os.File, *os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, nil, fmt.Errorf("create console directory: %w", err)
	}
	listener, err := listenUnix(path, "console")
	if err != nil {
		return nil, nil, nil, err
	}
	guestRead, hostWrite, err := os.Pipe()
	if err != nil {
		_ = listener.Close()
		return nil, nil, nil, fmt.Errorf("create console input pipe: %w", err)
	}
	hostRead, guestWrite, err := os.Pipe()
	if err != nil {
		_ = guestRead.Close()
		_ = hostWrite.Close()
		_ = listener.Close()
		return nil, nil, nil, fmt.Errorf("create console output pipe: %w", err)
	}

	proxy := startConsoleProxy(listener, hostWrite, hostRead, func() error {
		return errors.Join(hostWrite.Close(), hostRead.Close())
	})
	return proxy, guestRead, guestWrite, nil
}

func startConsoleProxy(listener *net.UnixListener, input io.Writer, output io.Reader, closeIO func() error) *consoleProxy {
	proxy := &consoleProxy{
		listener: listener,
		input:    input,
		output:   output,
		closeIO:  closeIO,
		ring:     newRingBuffer(consoleScrollback),
	}
	go proxy.accept()
	go proxy.writeOutput()
	return proxy
}

// listenUnix binds path, reclaiming it from a socket no process still answers
// on. label names the socket in every error the caller may surface.
func listenUnix(path, label string) (*net.UnixListener, error) {
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err == nil {
		return listener, nil
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("listen on %s socket: %w", label, err)
	}
	conn, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%s socket is already in use: %s", label, path)
	}
	if removeErr := os.Remove(path); removeErr != nil {
		return nil, fmt.Errorf("remove stale %s socket: %w", label, removeErr)
	}
	listener, err = net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s socket: %w", label, err)
	}
	return listener, nil
}

func (p *consoleProxy) accept() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.writeMu.Lock()
		if !p.setClient(conn) {
			p.writeMu.Unlock()
			_ = conn.SetWriteDeadline(time.Now().Add(consoleWriteTimeout))
			_, _ = conn.Write([]byte("console busy: another client is attached\n"))
			_ = conn.Close()
			continue
		}
		if scrollback := p.ring.Snapshot(); len(scrollback) > 0 {
			if !p.writeClient(conn, scrollback) {
				p.writeMu.Unlock()
				continue
			}
		}
		p.writeMu.Unlock()
		go func() {
			_, _ = io.Copy(p.input, conn)
			p.clearClient(conn)
		}()
	}
}

func (p *consoleProxy) writeOutput() {
	buf := make([]byte, 32*1024)
	for {
		n, err := p.output.Read(buf)
		if err != nil {
			return
		}
		p.ring.Write(buf[:n])
		p.writeLiveOutput(buf[:n])
	}
}

func (p *consoleProxy) writeLiveOutput(data []byte) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if conn := p.currentClient(); conn != nil {
		p.writeClient(conn, data)
	}
}

// writeClient is called with writeMu held so replay remains strictly ordered
// before live output. Never write when the timeout cannot be installed: a
// blocking write here would also block every future attach and replay.
func (p *consoleProxy) writeClient(conn net.Conn, data []byte) bool {
	if err := conn.SetWriteDeadline(time.Now().Add(consoleWriteTimeout)); err != nil {
		p.clearClient(conn)
		return false
	}
	if err := writeAll(conn, data); err != nil {
		p.clearClient(conn)
		return false
	}
	return true
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
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

func (p *consoleProxy) setClient(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return false
	}
	p.client = conn
	return true
}

func (p *consoleProxy) currentClient() net.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client
}

func (p *consoleProxy) clearClient(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == conn {
		p.client = nil
		_ = conn.Close()
	}
}

func (p *consoleProxy) close() {
	_ = p.listener.Close()
	if p.closeIO != nil {
		_ = p.closeIO()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
	}
}
