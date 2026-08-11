package main

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/helper"
)

func TestDomainSourceSnapshotDoesNoIO(t *testing.T) {
	t.Parallel()

	listCalls := 0
	source := newClusterDomainSource(func() ([]cluster.Cluster, error) {
		listCalls++
		return []cluster.Cluster{{Name: "demo"}, {Name: "lab", Domain: "lab.internal"}}, nil
	})

	// Until the first successful refresh the source fails closed.
	if got := source.snapshot(); !reflect.DeepEqual(got, []string{"*"}) {
		t.Fatalf("snapshot before refresh = %v, want [*]", got)
	}

	source.refresh()
	want := []string{"demo.k8s.test", "lab.internal"}
	if got := source.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	if listCalls != 1 {
		t.Fatalf("list calls = %d, want 1 (snapshot must not read state)", listCalls)
	}
	source.snapshot()
	source.snapshot()
	if listCalls != 1 {
		t.Fatalf("list calls = %d after repeated snapshots, want 1", listCalls)
	}
}

func TestDomainSourceRetainsLastKnownOnRefreshError(t *testing.T) {
	t.Parallel()

	fail := false
	source := newClusterDomainSource(func() ([]cluster.Cluster, error) {
		if fail {
			return nil, errors.New("state unreadable")
		}
		return []cluster.Cluster{{Name: "demo"}}, nil
	})
	source.refresh()
	fail = true
	source.refresh()
	if got := source.snapshot(); !reflect.DeepEqual(got, []string{"demo.k8s.test"}) {
		t.Fatalf("snapshot after failed refresh = %v, want last-known [demo.k8s.test]", got)
	}
}

func TestDNSReconcilerAddsReassertsAndRemovesClusterListeners(t *testing.T) {
	t.Parallel()

	client := &fakeClusterDNSClient{}
	servers := make(map[int]*fakeDNSServer)
	reconciler := newDNSReconciler(
		func(net.PacketConn, func(string) net.IP) dnsServing {
			server := &fakeDNSServer{}
			servers[len(servers)+7] = server
			return server
		},
		func(int, dnsServing) {},
	)
	lookup := func(string) net.IP { return nil }

	clusters := []cluster.Cluster{{Name: "demo", SubnetIndex: 7}}
	if err := reconciler.reconcile(client, clusters, lookup); err != nil {
		t.Fatal(err)
	}
	if len(client.listenCalls) != 1 || client.listenCalls[0] != "demo/demo.k8s.test/7" {
		t.Fatalf("listen calls = %v", client.listenCalls)
	}

	if err := reconciler.reconcile(client, clusters, lookup); err != nil {
		t.Fatal(err)
	}
	if len(client.listenCalls) != 1 || len(client.registerCalls) != 1 || client.registerCalls[0] != "demo/demo.k8s.test/7" {
		t.Fatalf("listen/register calls = %v / %v", client.listenCalls, client.registerCalls)
	}

	if err := reconciler.reconcile(client, nil, lookup); err != nil {
		t.Fatal(err)
	}
	if len(client.unregisterCalls) != 1 || client.unregisterCalls[0] != 7 {
		t.Fatalf("unregister calls = %v", client.unregisterCalls)
	}
	if server := servers[7]; server == nil || server.closeCalls != 1 {
		t.Fatalf("server after removal = %#v", server)
	}
}

type fakeClusterDNSClient struct {
	listenCalls     []string
	registerCalls   []string
	unregisterCalls []int
}

func (c *fakeClusterDNSClient) ListenDNS(clusterName, domain string, subnetIndex int) (net.PacketConn, helper.DNSRegistration, error) {
	c.listenCalls = append(c.listenCalls, fmt.Sprintf("%s/%s/%d", clusterName, domain, subnetIndex))
	return nil, helper.DNSRegistration{Registered: true}, nil
}

func (c *fakeClusterDNSClient) RegisterDNS(clusterName, domain string, subnetIndex int) (helper.DNSRegistration, error) {
	c.registerCalls = append(c.registerCalls, fmt.Sprintf("%s/%s/%d", clusterName, domain, subnetIndex))
	return helper.DNSRegistration{Registered: true}, nil
}

func (c *fakeClusterDNSClient) UnregisterDNS(subnetIndex int) error {
	c.unregisterCalls = append(c.unregisterCalls, subnetIndex)
	return nil
}

type fakeDNSServer struct{ closeCalls int }

func (*fakeDNSServer) Serve() error { return nil }

func (s *fakeDNSServer) Close() error {
	s.closeCalls++
	return nil
}
