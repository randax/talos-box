package helper

type dhcpManager interface {
	Converge() error
	// Release stops the DHCP server bound to a subnet's bridge, so a bridge
	// rebuilt under the same name is served by a freshly bound socket.
	Release(subnetIndex int) error
	Close() error
}

// ConvergeServices starts or refreshes helper-owned long-lived network
// services after the platform's bridge state has been converged.
func (s *Server) ConvergeServices() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.dhcp.Converge()
}
