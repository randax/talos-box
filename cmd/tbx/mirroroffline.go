package main

import (
	"fmt"
	"io"

	"github.com/randax/talos-box/internal/daemon"
)

// mirrorOfflineNotice is what status and doctor say while offline mode is on.
// The mode silently changes the failure mode of every pull on the host and
// survives a daemon restart, so an operator staring at ImagePullBackOff must be
// able to see it without remembering to ask `tbx mirror offline` (#403).
const mirrorOfflineNotice = "mirror offline is on: pulls are served from cache only and an uncached image fails; run `tbx mirror offline off` to restore upstream pulls"

// mirrorOfflineEnabled asks the daemon whether offline mode is on.
func (c cli) mirrorOfflineEnabled() (bool, error) {
	var result daemon.MirrorOfflineStatus
	if err := c.call("mirror.offline.get", struct{}{}, &result); err != nil {
		return false, err
	}
	return result.Enabled, nil
}

// printMirrorOfflineNotice heads a status listing with the offline banner. A
// daemon that cannot answer is not worth a second failure: status already
// reported what it could, so the banner is simply omitted.
func printMirrorOfflineNotice(w io.Writer, enabled bool, err error) error {
	if err != nil || !enabled {
		return nil
	}
	_, printErr := fmt.Fprintf(w, "%s\n\n", mirrorOfflineNotice)
	return printErr
}

// mirrorOfflineFinding reports the mode as its own doctor line.
func mirrorOfflineFinding(offline func() (bool, error)) doctorFinding {
	finding := doctorFinding{check: "mirror-offline"}
	if offline == nil {
		finding.level, finding.detail = "SKIP", "probe unavailable"
		return finding
	}
	enabled, err := offline()
	switch {
	case isDaemonUnavailable(err):
		finding.level, finding.detail = "SKIP", daemonUnavailableDetail(err)
	case err != nil:
		finding.level, finding.detail = "SKIP", err.Error()
	case enabled:
		finding.level, finding.detail = "WARN", mirrorOfflineNotice
	default:
		finding.level, finding.detail = "PASS", "off; pulls reach the upstream registry"
	}
	return finding
}
