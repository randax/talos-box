package main

// balloonDisabledFinding is the INFO line for a daemon that launches guests
// without a memory balloon device (TBX_DISABLE_BALLOON, added during the #513
// investigation; the panics turned out to be unrelated to the balloon — see
// docs/macos-panics.md). It is the operator's own choice, so
// there is nothing to print unless it is set: no PASS, no WARN, and a daemon
// that cannot be asked prints nothing either.
func balloonDisabledFinding(disabled func() (bool, error)) (doctorFinding, bool) {
	if disabled == nil {
		return doctorFinding{}, false
	}
	enabled, err := disabled()
	if err != nil || !enabled {
		return doctorFinding{}, false
	}
	return doctorFinding{
		level: "INFO",
		check: "balloon",
		detail: "daemon started with TBX_DISABLE_BALLOON: guests have no memory balloon device, " +
			"so their memory is never reclaimed under host pressure",
	}, true
}
