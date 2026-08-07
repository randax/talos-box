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
