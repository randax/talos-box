//go:build !darwin

package cluster

import "errors"

func cloneFile(_, _ string) error {
	return errors.New("clonefile is not supported on this platform")
}
