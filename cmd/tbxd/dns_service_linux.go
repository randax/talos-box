//go:build linux

package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	tbxdns "github.com/randax/talos-box/internal/dns"
	"github.com/randax/talos-box/internal/helper"
)

const (
	linuxDNSReconcileEvery     = time.Second
	linuxResolvedReassertEvery = time.Minute
)

type helperDNSClient struct{ client *helper.Client }

func (c helperDNSClient) ListenDNS(clusterName string, subnetIndex int) (net.PacketConn, helper.DNSRegistration, error) {
	return c.client.ListenDNS(clusterName, subnetIndex)
}

func (c helperDNSClient) RegisterDNS(clusterName string, subnetIndex int) (helper.DNSRegistration, error) {
	return c.client.RegisterDNS(clusterName, subnetIndex)
}

func (c helperDNSClient) UnregisterDNS(subnetIndex int) error {
	return c.client.UnregisterDNS(subnetIndex)
}

type dnsServeResult struct {
	subnetIndex int
	server      dnsServing
	err         error
}

type linuxDNSService struct {
	lookup                   func(string) net.IP
	reconciler               *dnsReconciler
	serveDone                chan dnsServeResult
	stop                     chan struct{}
	done                     chan struct{}
	closeOnce                sync.Once
	lastResolvedRegistration time.Time

	resultMu sync.Mutex
	result   error
}

func startDNSService(lookup func(string) net.IP) (daemonDNSService, error) {
	service := &linuxDNSService{
		lookup:    lookup,
		serveDone: make(chan dnsServeResult, 256),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	service.reconciler = newDNSReconciler(
		func(connection net.PacketConn, lookup func(string) net.IP) dnsServing {
			return tbxdns.NewServer(connection, lookup, tbxdns.SystemForward)
		},
		func(subnetIndex int, server dnsServing) {
			go func() {
				service.serveDone <- dnsServeResult{subnetIndex: subnetIndex, server: server, err: server.Serve()}
			}()
		},
	)
	go service.run()
	return service, nil
}

// Linux listener failures are recoverable: the reconciliation loop drops the
// failed socket and reacquires it from the helper. A nil channel explicitly
// disables main's fatal-DNS branch for this self-healing service.
func (s *linuxDNSService) Errors() <-chan error { return nil }

func (s *linuxDNSService) run() {
	defer close(s.done)
	ticker := time.NewTicker(linuxDNSReconcileEvery)
	defer ticker.Stop()

	s.reconcile(true)
	for {
		select {
		case <-ticker.C:
			reassert := time.Since(s.lastResolvedRegistration) >= linuxResolvedReassertEvery
			s.reconcile(reassert)
		case result := <-s.serveDone:
			current, exists := s.reconciler.listeners[result.subnetIndex]
			if !exists || current.server != result.server {
				continue
			}
			_ = current.server.Close()
			delete(s.reconciler.listeners, result.subnetIndex)
			if result.err != nil {
				log.Printf("DNS server on %s stopped; retrying: %v", cluster.Gateway(result.subnetIndex), result.err)
			}
		case <-s.stop:
			s.setResult(s.closeListeners())
			return
		}
	}
}

func (s *linuxDNSService) reconcile(reassertRegistration bool) {
	clusters, err := cluster.List()
	if err != nil {
		log.Printf("load clusters for DNS reconciliation: %v", err)
		return
	}
	if s.reconciler.matches(clusters) && !reassertRegistration {
		return
	}
	client, err := helper.Connect()
	if err != nil {
		if len(clusters) != 0 {
			log.Printf("connect to helper for DNS reconciliation: %v", err)
		}
		return
	}
	defer func() { _ = client.Close() }()
	if err := s.reconciler.reconcileWithRegistration(
		helperDNSClient{client: client}, clusters, s.lookup, reassertRegistration,
	); err != nil {
		log.Printf("reconcile cluster DNS: %v", err)
	}
	if reassertRegistration {
		s.lastResolvedRegistration = time.Now()
	}
}

func (r *dnsReconciler) matches(clusters []cluster.Cluster) bool {
	if len(r.listeners) != len(clusters) {
		return false
	}
	for _, item := range clusters {
		current, exists := r.listeners[item.SubnetIndex]
		if !exists || current.cluster != item.Name {
			return false
		}
	}
	return true
}

func (r *dnsReconciler) close(client clusterDNSClient) error {
	var result error
	for _, subnetIndex := range sortedListenerIndexes(r.listeners) {
		current := r.listeners[subnetIndex]
		result = errors.Join(result, current.server.Close())
		if err := client.UnregisterDNS(subnetIndex); err != nil {
			result = errors.Join(result, fmt.Errorf("unregister DNS for subnet %d: %w", subnetIndex, err))
		}
		delete(r.listeners, subnetIndex)
	}
	return result
}

func (s *linuxDNSService) closeListeners() error {
	client, err := helper.Connect()
	if err != nil {
		// Resolver mutations stay behind the privileged helper boundary. The
		// error makes incomplete cleanup visible; a restarted daemon overwrites
		// active per-link registrations during its first reconciliation.
		return errors.Join(fmt.Errorf("connect to helper while closing DNS: %w", err), s.closeServers())
	}
	defer func() { _ = client.Close() }()
	return s.reconciler.close(helperDNSClient{client: client})
}

func (s *linuxDNSService) closeServers() error {
	var result error
	for _, subnetIndex := range sortedListenerIndexes(s.reconciler.listeners) {
		current := s.reconciler.listeners[subnetIndex]
		result = errors.Join(result, current.server.Close())
		delete(s.reconciler.listeners, subnetIndex)
	}
	return result
}

func (s *linuxDNSService) setResult(err error) {
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	s.result = err
}

func (s *linuxDNSService) Close() error {
	s.closeOnce.Do(func() { close(s.stop) })
	<-s.done
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	return s.result
}
