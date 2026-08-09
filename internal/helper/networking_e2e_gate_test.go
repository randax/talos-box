package helper

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func parseLaunchctlJobPID(output string) (int, error) {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "pid = ") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "pid = ")))
		if err != nil || pid <= 0 {
			return 0, fmt.Errorf("parse launchctl pid from %q", trimmed)
		}
		return pid, nil
	}
	return 0, fmt.Errorf("launchctl output did not include a pid line")
}

func TestParseLaunchctlJobPID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		output  string
		want    int
		wantErr bool
	}{
		{name: "happy path", output: "service = dev.talosbox.helper\npid = 1234\nstate = running\n", want: 1234},
		{name: "trimmed line", output: "\tpid = 77\n", want: 77},
		{name: "missing", output: "state = waiting\n", wantErr: true},
		{name: "zero", output: "pid = 0\n", wantErr: true},
		{name: "garbage", output: "pid = abc\n", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLaunchctlJobPID(test.output)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseLaunchctlJobPID() = %d, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLaunchctlJobPID() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseLaunchctlJobPID() = %d, want %d", got, test.want)
			}
		})
	}
}
