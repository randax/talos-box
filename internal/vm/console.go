package vm

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const consoleWriteTimeout = 2 * time.Second

// consoleScrollback is how much recent guest output an attach replays.
const consoleScrollback = 64 * 1024

type consoleProxy struct {
	listener *net.UnixListener
	input    *os.File
	output   *os.File
	ring     *ringBuffer

	mu     sync.Mutex
	client net.Conn

	// writeMu serializes attach-replay with live output so a new client sees
	// scrollback strictly before anything the guest writes afterwards.
	writeMu sync.Mutex
}

func newConsoleProxy(path string) (*consoleProxy, *os.File, *os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, nil, fmt.Errorf("create console directory: %w", err)
	}

	listener, err := listenUnix(path)
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

	proxy := &consoleProxy{listener: listener, input: hostWrite, output: hostRead, ring: newRingBuffer(consoleScrollback)}
	go proxy.accept()
	go proxy.writeOutput()

	return proxy, guestRead, guestWrite, nil
}

func listenUnix(path string) (*net.UnixListener, error) {
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err == nil {
		return listener, nil
	}

	info, statErr := os.Lstat(path)
	if statErr != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("listen on console socket: %w", err)
	}
	conn, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("console socket is already in use: %s", path)
	}
	if removeErr := os.Remove(path); removeErr != nil {
		return nil, fmt.Errorf("remove stale console socket: %w", removeErr)
	}

	listener, err = net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on console socket: %w", err)
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
			_ = conn.SetWriteDeadline(time.Now().Add(consoleWriteTimeout))
			if err := writeAll(conn, scrollback); err != nil {
				p.clearClient(conn)
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
		p.writeMu.Lock()
		conn := p.currentClient()
		if conn == nil {
			p.writeMu.Unlock()
			continue
		}
		if err := conn.SetWriteDeadline(time.Now().Add(consoleWriteTimeout)); err != nil {
			p.clearClient(conn)
			p.writeMu.Unlock()
			continue
		}
		if err := writeAll(conn, buf[:n]); err != nil {
			p.clearClient(conn)
		}
		p.writeMu.Unlock()
	}
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
	_ = p.input.Close()
	_ = p.output.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
	}
}
