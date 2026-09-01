package main

import (
	"fmt"
	"strings"

	"github.com/randax/talos-box/internal/wsl"
)

// wslDoctorFinding renders host identity only. Capability verdicts such as
// mirrored-mode reachability belong to the checks that exercise them; this
// inventory can never warn, fail, or change doctor's exit status (#553).
func wslDoctorFinding(identity wsl.Identity) (doctorFinding, bool) {
	if !identity.IsWSL() {
		return doctorFinding{}, false
	}
	generation := "WSL1"
	if identity.IsWSL2() {
		generation = "WSL2"
	}
	version := generation + " version unreadable"
	if identity.WSLVersion.Err == nil && identity.WSLVersion.Value != "" {
		version = generation + " " + identity.WSLVersion.Value
	}
	clauses := []string{
		version,
		observationClause("distro", identity.Distribution, "distro unreadable"),
		observationClause("Windows build", identity.WindowsBuild, "Windows side unreadable"),
		observationClause("networking mode", identity.NetworkingMode, "networking mode unreadable"),
		observationClause("NAT prefix", identity.NATPrefix, "NAT prefix unreadable"),
	}
	return doctorFinding{level: "INFO", check: "wsl", detail: strings.Join(clauses, "; ")}, true
}

func observationClause(label string, observation wsl.Observation, fallback string) string {
	if observation.Err != nil || observation.Value == "" {
		return fallback
	}
	return fmt.Sprintf("%s %s", label, observation.Value)
}
