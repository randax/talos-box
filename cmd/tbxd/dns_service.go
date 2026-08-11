package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/helper"
)

// clusterDomainSource maintains an in-memory snapshot of the cluster domain
// set for the DNS authority predicate, so answering a query never touches
// disk. refresh() reads state (retaining the last good set on error — a
// transient state error must never shrink the set, or queries for live
// custom domains would be forwarded to the upstream resolver, leaking names
// or answering from real DNS for unsafe domains). Until the first successful
// refresh, snapshot() fails closed with the "*" sentinel, which Authority
// treats as claiming every name.
type clusterDomainSource struct {
	list func() ([]cluster.Cluster, error)

	mu       sync.Mutex
	last     []string
	everRead bool
}

func newClusterDomainSource(list func() ([]cluster.Cluster, error)) *clusterDomainSource {
	return &clusterDomainSource{list: list}
}

// snapshot returns the current domain set without any IO.
func (s *clusterDomainSource) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.everRead {
		return []string{"*"}
	}
	return s.last
}

// refresh re-reads cluster state into the snapshot. The read happens under
// the lock so two overlapping refreshes cannot store out of order and
// regress the set to an older read.
func (s *clusterDomainSource) refresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clusters, err := s.list()
	if err != nil {
		if !s.everRead {
			log.Printf("DNS state has never been readable; answering all queries locally: %v", err)
			return
		}
		log.Printf("DNS state refresh failed; keeping last-known domain set: %v", err)
		return
	}
	domains := make([]string, 0, len(clusters))
	for _, item := range clusters {
		domains = append(domains, item.EffectiveDomain())
	}
	s.last, s.everRead = domains, true
}

// refreshEvery keeps the snapshot fresh in the background until the returned
// stop function is called.
func (s *clusterDomainSource) refreshEvery(interval time.Duration) func() {
	ticker := time.NewTicker(interval)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.refresh()
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(stop)
		<-done
	}
}

// domainRefreshEvery is the background cadence for the authority snapshot; a
// created or destroyed cluster's domain is authoritative within this bound.
const domainRefreshEvery = time.Second

type daemonDNSService interface {
	Errors() <-chan error
	Close() error
}

type dnsServing interface {
	Serve() error
	Close() error
}

type clusterDNSClient interface {
	ListenDNS(clusterName, domain string, subnetIndex int) (net.PacketConn, helper.DNSRegistration, error)
	RegisterDNS(clusterName, domain string, subnetIndex int) (helper.DNSRegistration, error)
	UnregisterDNS(subnetIndex int) error
}

type managedDNSListener struct {
	cluster string
	server  dnsServing
}

type dnsReconciler struct {
	listeners map[int]managedDNSListener
	newServer func(net.PacketConn, func(string) net.IP) dnsServing
	launch    func(int, dnsServing)
}

func newDNSReconciler(
	newServer func(net.PacketConn, func(string) net.IP) dnsServing,
	launch func(int, dnsServing),
) *dnsReconciler {
	return &dnsReconciler{
		listeners: make(map[int]managedDNSListener),
		newServer: newServer,
		launch:    launch,
	}
}

func (r *dnsReconciler) reconcile(client clusterDNSClient, clusters []cluster.Cluster, lookup func(string) net.IP) error {
	return r.reconcileWithRegistration(client, clusters, lookup, true)
}

func (r *dnsReconciler) reconcileWithRegistration(
	client clusterDNSClient,
	clusters []cluster.Cluster,
	lookup func(string) net.IP,
	reassertRegistration bool,
) error {
	desired := make(map[int]string, len(clusters))
	for _, item := range clusters {
		if previous, exists := desired[item.SubnetIndex]; exists {
			return fmt.Errorf("clusters %q and %q share subnet index %d", previous, item.Name, item.SubnetIndex)
		}
		desired[item.SubnetIndex] = item.Name
	}

	var result error
	for _, subnetIndex := range sortedListenerIndexes(r.listeners) {
		current := r.listeners[subnetIndex]
		if clusterName, exists := desired[subnetIndex]; exists && clusterName == current.cluster {
			continue
		}
		result = errors.Join(result, current.server.Close())
		if err := client.UnregisterDNS(subnetIndex); err != nil {
			result = errors.Join(result, fmt.Errorf("unregister DNS for subnet %d: %w", subnetIndex, err))
		}
		delete(r.listeners, subnetIndex)
	}

	for _, item := range clusters {
		if _, exists := r.listeners[item.SubnetIndex]; exists {
			if !reassertRegistration {
				continue
			}
			registration, err := client.RegisterDNS(item.Name, item.EffectiveDomain(), item.SubnetIndex)
			if err != nil {
				result = errors.Join(result, fmt.Errorf("register DNS for %s: %w", item.Name, err))
			} else {
				logDNSRegistration(item.Name, registration)
			}
			continue
		}

		connection, registration, err := client.ListenDNS(item.Name, item.EffectiveDomain(), item.SubnetIndex)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("listen for DNS on %s: %w", cluster.Gateway(item.SubnetIndex), err))
			continue
		}
		server := r.newServer(connection, lookup)
		r.listeners[item.SubnetIndex] = managedDNSListener{cluster: item.Name, server: server}
		r.launch(item.SubnetIndex, server)
		logDNSRegistration(item.Name, registration)
	}
	return result
}

func sortedListenerIndexes(listeners map[int]managedDNSListener) []int {
	indexes := make([]int, 0, len(listeners))
	for index := range listeners {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}

func logDNSRegistration(clusterName string, registration helper.DNSRegistration) {
	if registration.Registered {
		return
	}
	detail := registration.Detail
	if registration.ManualStep != "" {
		detail += "; manual step: " + registration.ManualStep
	}
	log.Printf("host DNS for cluster %s is unavailable; guest and by-IP access remain available: %s", clusterName, detail)
}
