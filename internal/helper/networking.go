package helper

import (
	"errors"
	"fmt"
)

// ConvergeNetworking re-asserts all helper-owned host networking state for the
// subnets the synced reservations occupy. On Linux this is the ordinary boot
// and helper-restart path; other platforms have no persistent host state to
// restore.
func ConvergeNetworking(subnets []int) error {
	return convergeNetworking(subnets)
}

// isSubnetPreflightError reports whether err only records subnets left
// unconverged by their preflight; everything else was converged.
func isSubnetPreflightError(err error) bool {
	var preflight *SubnetPreflightError
	return errors.As(err, &preflight)
}

// SubnetPreflightError reports subnets convergence left alone because their
// preflight failed (a foreign route over the subnet, say). The remaining
// subnets were converged: one captured subnet must not take every other
// cluster's host networking down with it, and the cluster on the captured
// subnet learns the conflict from its own attach.
type SubnetPreflightError struct {
	Skipped []int
	Err     error
}

func (e *SubnetPreflightError) Error() string {
	return fmt.Sprintf("subnets %v were not converged: %v", e.Skipped, e.Err)
}

func (e *SubnetPreflightError) Unwrap() error { return e.Err }
