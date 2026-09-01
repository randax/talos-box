//go:build linux

package main

import (
	"reflect"
	"testing"

	"github.com/randax/talos-box/internal/wsl"
)

func TestLinuxPlatformNamesForHelpMatchProduction(t *testing.T) {
	t.Parallel()

	for _, generation := range []wsl.Generation{wsl.NotWSL, wsl.WSL2} {
		if got, want := linuxPlatformNamesForHelp(generation), linuxPlatformDoctorCheckNames(generation); !reflect.DeepEqual(got, want) {
			t.Errorf("generation %v help names = %v, want %v", generation, got, want)
		}
	}
}
