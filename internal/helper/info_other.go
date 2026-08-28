//go:build !linux

package helper

import (
	"os"

	"github.com/randax/talos-box/internal/version"
)

func currentHelperInfo() (Info, int, func(), error) {
	return Info{
		ProtocolVersion: ProtocolVersion,
		Version:         version.Version,
		Executable:      helperExecutable(),
		PID:             os.Getpid(),
	}, -1, nil, nil
}

func helperExecutable() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	return executable
}
