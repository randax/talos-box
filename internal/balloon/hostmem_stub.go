//go:build !darwin

package balloon

import "context"

func HostTotalMiB() (int, error) { return 0, ErrUnsupported }
func HostFreeMiB() (int, error)  { return 0, ErrUnsupported }

func HostFreeMiBContext(context.Context) (int, error) { return 0, ErrUnsupported }
