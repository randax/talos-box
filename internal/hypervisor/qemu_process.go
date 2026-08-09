package hypervisor

import (
	"errors"
	"os"
	"os/exec"
	"sync"
)

// qemuProcess owns the single Wait call for a QEMU child. Callers observe the
// closed done channel instead of racing on ProcessState or issuing another
// Wait, so unexpected child death is reflected immediately and never leaks a
// zombie.
type qemuProcess struct {
	process *os.Process
	done    chan struct{}

	waitMu  sync.Mutex
	waitErr error
}

func startQEMUProcess(command *exec.Cmd) (*qemuProcess, error) {
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &qemuProcess{process: command.Process, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		process.waitMu.Lock()
		process.waitErr = err
		process.waitMu.Unlock()
		close(process.done)
	}()
	return process, nil
}

func (p *qemuProcess) active() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *qemuProcess) waitError() error {
	if p == nil {
		return nil
	}
	<-p.done
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waitErr
}

func (p *qemuProcess) kill() error {
	if p == nil || !p.active() {
		return nil
	}
	err := p.process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
