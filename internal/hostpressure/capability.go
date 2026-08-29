package hostpressure

import "github.com/randax/talos-box/internal/hostmem"

// ErrUnsupported reports that this platform has no host-pressure probe at all.
// A missing capability is not a failed measurement: `tbx doctor` reports it as
// SKIP, and the provision gates match it with errors.Is so they stand down
// silently instead of warning on every operation (#446).
var ErrUnsupported = hostmem.ErrUnsupported
