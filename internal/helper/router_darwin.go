//go:build darwin && cgo

package helper

func (r *frameRouter) removePort(port *routerPort) {
	if port == nil {
		return
	}

	r.mu.Lock()

	current, ok := r.ports[port.id]
	if !ok || current != port {
		r.mu.Unlock()
		return
	}
	delete(r.ports, port.id)
	for ip := range port.ips {
		if owner := r.ipToPort[ip]; owner == port {
			delete(r.ipToPort, ip)
		}
	}
	r.mu.Unlock()

	port.closeSend()
}

func (p *routerPort) closeSend() {
	p.sendMu.Lock()
	p.closed = true
	p.sendMu.Unlock()
}
