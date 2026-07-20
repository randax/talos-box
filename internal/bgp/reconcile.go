// Package bgp reconciles routes learned over BGP into the host FIB. The
// speaker (GoBGP) and the FIB implementation live in the root helper, since
// binding BGP's port 179 and injecting routes both require privilege; this
// package holds the pure reconciliation logic, tested against a fake FIB.
package bgp

import (
	"errors"
	"fmt"
	"sync"
)

// Route is a learned path: an LB VIP /32 and the node advertising it.
type Route struct {
	Prefix  string
	Nexthop string
}

// FIB is the host routing table as the reconciler needs it.
type FIB interface {
	AddHostRoute(prefix, nexthop string) error
	DeleteHostRoute(prefix string) error
}

// Reconciler drives the FIB toward a desired route set, tracking exactly what
// it has injected so it can update nexthops and withdraw vanished routes.
type Reconciler struct {
	fib      FIB
	mu       sync.Mutex        // guards injected against the watch callback + ticker
	injected map[string]string // prefix -> nexthop currently in the FIB
}

// NewReconciler returns a Reconciler over fib with an empty injected set.
func NewReconciler(fib FIB) *Reconciler {
	return &Reconciler{fib: fib, injected: map[string]string{}}
}

// Reconcile makes the FIB match desired: add new prefixes, update changed
// nexthops, delete prefixes no longer present. A prefix that fails to add is
// left out of the injected set so the next reconcile retries it.
func (r *Reconciler) Reconcile(desired []Route) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := make(map[string]string, len(desired))
	for _, route := range desired {
		want[route.Prefix] = route.Nexthop
	}

	var errs []error
	for prefix, nexthop := range want {
		if current, ok := r.injected[prefix]; ok && current == nexthop {
			continue
		}
		if _, ok := r.injected[prefix]; ok {
			// nexthop changed: replace the route
			_ = r.fib.DeleteHostRoute(prefix)
			delete(r.injected, prefix)
		}
		if err := r.fib.AddHostRoute(prefix, nexthop); err != nil {
			errs = append(errs, fmt.Errorf("add %s: %w", prefix, err))
			continue
		}
		r.injected[prefix] = nexthop
	}
	for prefix := range r.injected {
		if _, ok := want[prefix]; !ok {
			_ = r.fib.DeleteHostRoute(prefix)
			delete(r.injected, prefix)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// WithdrawAll removes every injected route — called when the BGP session ends
// or the cluster's BGP mode is disabled.
func (r *Reconciler) WithdrawAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for prefix := range r.injected {
		_ = r.fib.DeleteHostRoute(prefix)
	}
	r.injected = map[string]string{}
}
