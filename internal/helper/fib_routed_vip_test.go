package helper

import (
	"errors"
	"testing"
)

type fakeFIB struct {
	added   map[string]string
	deleted []string
	addErr  error
	delErr  error
}

func (f *fakeFIB) AddHostRoute(prefix, nexthop string) error {
	if f.addErr != nil {
		return f.addErr
	}
	if f.added == nil {
		f.added = map[string]string{}
	}
	f.added[prefix] = nexthop
	return nil
}

func (f *fakeFIB) DeleteHostRoute(prefix string) error {
	f.deleted = append(f.deleted, prefix)
	return f.delErr
}

func TestRoutedVIPFIBBindsOnlyRoutesTheKernelAccepted(t *testing.T) {
	router := newFrameRouter()
	fib := &fakeFIB{addErr: errors.New("route add: File exists")}
	routed := newRoutedVIPFIB(fib, router)

	if err := routed.AddHostRoute("172.30.241.200/32", "172.30.241.68"); err == nil {
		t.Fatal("AddHostRoute must surface the underlying FIB error")
	}
	if len(router.routedVIPs) != 0 {
		t.Fatalf("routedVIPs = %v, want nothing bound after a failed route add", router.routedVIPs)
	}

	fib.addErr = nil
	if err := routed.AddHostRoute("172.30.241.200/32", "172.30.241.68"); err != nil {
		t.Fatal(err)
	}
	if fib.added["172.30.241.200/32"] != "172.30.241.68" {
		t.Fatalf("kernel routes = %v, want the host route injected", fib.added)
	}
	if len(router.routedVIPs) != 1 {
		t.Fatalf("routedVIPs = %v, want the VIP bound to its nexthop", router.routedVIPs)
	}

	fib.delErr = errors.New("route delete: not in table")
	if err := routed.DeleteHostRoute("172.30.241.200/32"); err == nil {
		t.Fatal("DeleteHostRoute must surface the underlying FIB error")
	}
	if len(router.routedVIPs) != 0 {
		t.Fatalf("routedVIPs = %v, want the VIP unbound even when the kernel delete failed", router.routedVIPs)
	}
}
