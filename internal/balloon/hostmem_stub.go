//go:build !darwin

package balloon

import (
	"context"

	"github.com/randax/talos-box/internal/hostmem"
)

func HostTotalMiB() (int, error) { return 0, hostmem.ErrUnsupported }
func HostFreeMiB() (int, error)  { return 0, hostmem.ErrUnsupported }

func HostFreeMiBContext(context.Context) (int, error) { return 0, hostmem.ErrUnsupported }
