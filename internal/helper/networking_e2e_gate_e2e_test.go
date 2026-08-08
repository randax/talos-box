//go:build e2e

package helper

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	helperLaunchdLabel = "dev.talosbox.helper"
	helperE2EPIDEnv    = "TBX_HELPER_E2E_PID"
)

func requiredHelperE2EPID() (int, error) {
	value := os.Getenv(helperE2EPIDEnv)
	if value == "" {
		return 0, fmt.Errorf("%s is unset; rebuild/restart %s and export the active pid before running helper networking e2e", helperE2EPIDEnv, helperLaunchdLabel)
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%s=%q is invalid; expected a positive integer pid", helperE2EPIDEnv, value)
	}
	return pid, nil
}

func activeHelperPID() (int, error) {
	output, err := exec.Command("/bin/launchctl", "print", "system/"+helperLaunchdLabel).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("launchctl print system/%s: %w: %s", helperLaunchdLabel, err, strings.TrimSpace(string(output)))
	}
	return parseLaunchctlJobPID(string(output))
}
