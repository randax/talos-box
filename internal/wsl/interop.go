package wsl

import (
	"errors"
	"fmt"
	"strings"
)

const windowsCurrentVersionKey = `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`

type commandWindowsInterop struct {
	command Command
}

// NewWindowsInterop owns both the reg.exe invocation and its output parsing;
// no other package code crosses the Windows boundary directly (#553).
func NewWindowsInterop(command Command) WindowsInterop {
	return commandWindowsInterop{command: command}
}

func (i commandWindowsInterop) WindowsBuild() (string, error) {
	if i.command == nil {
		return "", errors.New("no command runner for the Windows side")
	}
	output, err := i.command("reg.exe", "query", windowsCurrentVersionKey, "/v", "CurrentBuildNumber")
	if err != nil {
		return "", fmt.Errorf("query Windows build: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 3 && fields[0] == "CurrentBuildNumber" && strings.HasPrefix(fields[1], "REG_") {
			return strings.Join(fields[2:], " "), nil
		}
	}
	return "", errors.New("CurrentBuildNumber is missing or malformed")
}
