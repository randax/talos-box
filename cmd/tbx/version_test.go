package main

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/version"
)

func TestVersionPrintsRuntimeIdentityBlock(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		out := &bytes.Buffer{}
		deps := runtimeIdentityDeps{
			executable: func() (string, error) { return "/opt/current/tbx", nil },
			daemonProbe: func(context.Context) (daemon.Info, int, error) {
				return daemon.Info{ProtocolVersion: daemon.ProtocolVersion, Version: "0.1.3", Executable: "/opt/current/tbxd", PID: 1234}, 1234, nil
			},
			helperProbe: func(context.Context) (helper.Info, error) {
				return helper.Info{ProtocolVersion: helper.ProtocolVersion, Version: "0.1.3", Executable: "/opt/current/tbx-helper", PID: 2345}, nil
			},
		}
		c := cli{out: out, err: out, runtimeIdentityDeps: &deps}
		if err := c.run(args); err != nil {
			t.Fatalf("tbx %s: %v", strings.Join(args, " "), err)
		}
		text := out.String()
		for _, wanted := range []string{
			"tbx " + version.Version + " (" + runtime.GOOS + "/" + runtime.GOARCH,
			fmt.Sprintf("daemon protocol %d", daemon.ProtocolVersion),
			fmt.Sprintf("helper protocol %d", helper.ProtocolVersion),
			"runtime:\n",
			"client: /opt/current/tbx",
			"daemon: /opt/current/tbxd (0.1.3",
			"helper: /opt/current/tbx-helper (0.1.3",
		} {
			if !strings.Contains(text, wanted) {
				t.Errorf("tbx %s output missing %q:\n%s", strings.Join(args, " "), wanted, text)
			}
		}
	}
}
