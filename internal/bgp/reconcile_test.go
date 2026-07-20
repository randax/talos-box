package bgp

import (
	"errors"
	"reflect"
	"sort"
	"testing"
)

// fakeFIB records add/delete calls and can be made to fail.
type fakeFIB struct {
	routes map[string]string // prefix -> nexthop
	addErr error
	addLog []string
	delLog []string
}

func newFakeFIB() *fakeFIB { return &fakeFIB{routes: map[string]string{}} }

func (f *fakeFIB) AddHostRoute(prefix, nexthop string) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.routes[prefix] = nexthop
	f.addLog = append(f.addLog, prefix+"->"+nexthop)
	return nil
}

func (f *fakeFIB) DeleteHostRoute(prefix string) error {
	delete(f.routes, prefix)
	f.delLog = append(f.delLog, prefix)
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestReconcileInjectsNewRoutes(t *testing.T) {
	fib := newFakeFIB()
	r := NewReconciler(fib)
	err := r.Reconcile([]Route{
		{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.2"},
		{Prefix: "172.30.0.201/32", Nexthop: "172.30.0.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"172.30.0.200/32": "172.30.0.2", "172.30.0.201/32": "172.30.0.3"}
	if !reflect.DeepEqual(fib.routes, want) {
		t.Errorf("routes = %v, want %v", fib.routes, want)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	fib := newFakeFIB()
	r := NewReconciler(fib)
	routes := []Route{{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.2"}}
	_ = r.Reconcile(routes)
	_ = r.Reconcile(routes)
	if len(fib.addLog) != 1 {
		t.Errorf("re-reconcile re-added routes: %v", fib.addLog)
	}
}

func TestReconcileWithdrawsVanishedRoutes(t *testing.T) {
	fib := newFakeFIB()
	r := NewReconciler(fib)
	_ = r.Reconcile([]Route{
		{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.2"},
		{Prefix: "172.30.0.201/32", Nexthop: "172.30.0.3"},
	})
	// .201 is withdrawn by the peer
	_ = r.Reconcile([]Route{{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.2"}})
	if _, ok := fib.routes["172.30.0.201/32"]; ok {
		t.Error("withdrawn route still in FIB")
	}
	if got := sortedKeys(fib.routes); !reflect.DeepEqual(got, []string{"172.30.0.200/32"}) {
		t.Errorf("routes after withdrawal = %v", got)
	}
}

func TestReconcileMovesNexthop(t *testing.T) {
	fib := newFakeFIB()
	r := NewReconciler(fib)
	_ = r.Reconcile([]Route{{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.2"}})
	// VIP failover: same prefix, new node
	_ = r.Reconcile([]Route{{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.3"}})
	if fib.routes["172.30.0.200/32"] != "172.30.0.3" {
		t.Errorf("nexthop not updated: %v", fib.routes)
	}
}

func TestWithdrawAllClearsInjectedRoutes(t *testing.T) {
	fib := newFakeFIB()
	r := NewReconciler(fib)
	_ = r.Reconcile([]Route{
		{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.2"},
		{Prefix: "172.30.0.201/32", Nexthop: "172.30.0.3"},
	})
	r.WithdrawAll()
	if len(fib.routes) != 0 {
		t.Errorf("WithdrawAll left routes: %v", fib.routes)
	}
	// and a subsequent reconcile treats everything as new again
	_ = r.Reconcile([]Route{{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.2"}})
	if fib.routes["172.30.0.200/32"] != "172.30.0.2" {
		t.Error("reconcile after WithdrawAll did not re-inject")
	}
}

func TestReconcileReportsAddFailure(t *testing.T) {
	fib := newFakeFIB()
	fib.addErr = errors.New("route: permission denied")
	r := NewReconciler(fib)
	if err := r.Reconcile([]Route{{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.2"}}); err == nil {
		t.Fatal("expected reconcile to report FIB add failure")
	}
	// a failed add must NOT be recorded as injected, so a retry tries again
	fib.addErr = nil
	_ = r.Reconcile([]Route{{Prefix: "172.30.0.200/32", Nexthop: "172.30.0.2"}})
	if fib.routes["172.30.0.200/32"] != "172.30.0.2" {
		t.Error("route not retried after earlier failure")
	}
}
