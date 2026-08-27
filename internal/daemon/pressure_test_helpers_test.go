package daemon

import "github.com/randax/talos-box/internal/hostpressure"

func extremeSwapPressure(string) (hostpressure.Snapshot, error) {
	return hostpressure.Snapshot{
		Swap: hostpressure.Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
	}, nil
}

func noHostPressure(string) (hostpressure.Snapshot, error) {
	return hostpressure.Snapshot{}, nil
}

// plentifulHostMemory pins the overcommit gate open: tests that exercise start
// paths for other reasons must not depend on the runner's real RAM.
func plentifulHostMemory() (int, error) {
	return 1 << 20, nil
}

// scarceHostMemory keeps swap-refusal fixtures on the low-headroom side of
// #483's rule without consulting the machine running the test.
func scarceHostMemory() (int, error) {
	return 1024, nil
}
