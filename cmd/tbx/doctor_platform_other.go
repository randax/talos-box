//go:build !darwin && !linux

package main

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
