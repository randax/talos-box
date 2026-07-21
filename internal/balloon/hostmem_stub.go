//go:build !darwin

package balloon

import "errors"

var errUnsupported = errors.New("host memory reading is only implemented on macOS")

func HostTotalMiB() (int, error) { return 0, errUnsupported }
func HostFreeMiB() (int, error)  { return 0, errUnsupported }
