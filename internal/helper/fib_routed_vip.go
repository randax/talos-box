package helper

import "github.com/randax/talos-box/internal/bgp"

// routedVIPFIB mirrors the BGP speaker's host-route writes into the helper's
// frame router. The kernel FIB alone only serves host-originated traffic; on
// macOS every guest-to-guest packet crosses subnets through the userspace
// router instead, and that router has no other way to learn a VIP that is
// announced over BGP rather than ARPed for on the segment (#387).
type routedVIPFIB struct {
	fib    bgp.FIB
	router *frameRouter
}

func newRoutedVIPFIB(fib bgp.FIB, router *frameRouter) routedVIPFIB {
	return routedVIPFIB{fib: fib, router: router}
}

// AddHostRoute binds the VIP only once the kernel route is in place, so the
// two views of reachability never disagree: a route the host itself cannot use
// is not one sibling clusters should be forwarded onto.
func (f routedVIPFIB) AddHostRoute(prefix, nexthop string) error {
	if err := f.fib.AddHostRoute(prefix, nexthop); err != nil {
		return err
	}
	f.router.learnRoutedVIP(prefix, nexthop)
	return nil
}

// DeleteHostRoute unbinds first, for the same reason in reverse: a withdrawn
// VIP must stop being forwarded even if the kernel route removal fails.
func (f routedVIPFIB) DeleteHostRoute(prefix string) error {
	f.router.forgetRoutedVIP(prefix)
	return f.fib.DeleteHostRoute(prefix)
}
