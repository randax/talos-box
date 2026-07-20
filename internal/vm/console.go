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

type consoleProxy struct {
	listener *net.UnixListener
	input    *os.File
	output   *os.File

	mu     sync.Mutex
	client net.Conn
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

	proxy := &consoleProxy{listener: listener, input: hostWrite, output: hostRead}
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
		if !p.setClient(conn) {
			_ = conn.Close()
			continue
		}
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
		conn := p.currentClient()
		if conn == nil {
			continue
		}
		if err := conn.SetWriteDeadline(time.Now().Add(consoleWriteTimeout)); err != nil {
			p.clearClient(conn)
			continue
		}
		if err := writeAll(conn, buf[:n]); err != nil {
			p.clearClient(conn)
		}
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
