package helper

type dhcpManager interface {
	Converge() error
	Close() error
}

// ConvergeServices starts or refreshes helper-owned long-lived network
// services after the platform's bridge state has been converged.
func (s *Server) ConvergeServices() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.dhcp.Converge()
}
