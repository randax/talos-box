//go:build !linux

package main

import (
	"errors"
	"net"
	"sync"

	tbxdns "github.com/randax/talos-box/internal/dns"
)

type singleDNSService struct {
	server dnsServing
	errors chan error
	done   chan struct{}

	mu     sync.Mutex
	result error
}

func startSingleDNSService(server dnsServing) *singleDNSService {
	service := &singleDNSService{
		server: server,
		errors: make(chan error, 1),
		done:   make(chan struct{}),
	}
	go func() {
		err := server.Serve()
		service.mu.Lock()
		service.result = err
		service.mu.Unlock()
		service.errors <- err
		close(service.done)
	}()
	return service
}

func (s *singleDNSService) Errors() <-chan error { return s.errors }

func (s *singleDNSService) Close() error {
	closeErr := s.server.Close()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(closeErr, s.result)
}

func startDNSService(lookup func(string) net.IP) (daemonDNSService, error) {
	server, err := tbxdns.Listen(tbxdns.Address, lookup)
	if err != nil {
		return nil, err
	}
	return startSingleDNSService(server), nil
}
