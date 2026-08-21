//go:build !darwin && !linux

package main

// platformDoctorCheckNames is empty here: this platform adds no checks of its
// own, so `tbx doctor --help` lists only the portable ones (#419).
func platformDoctorCheckNames() []string { return nil }

func platformDoctorDependencies(deps *doctorDependencies) {
	if deps == nil {
		return
	}
	probe := deps.checkDirectDNS
	listClusters := deps.listClusters
	deps.checkDirectDNS = func() error {
		return directDNSDaemonSkip(probe, listClusters)
	}
}
