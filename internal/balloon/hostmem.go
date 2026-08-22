package balloon

import "errors"

// ErrUnsupported reports that this platform has no host-memory probe at all —
// a missing capability, not a failed reading. Callers match it with errors.Is
// to stand down once instead of treating every poll as a probe failure (#446).
var ErrUnsupported = errors.New("host memory reading is only implemented on macOS")
