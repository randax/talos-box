package main

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/version"
)

func TestVersionPrintsProductPlatformAndProtocol(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		out := &bytes.Buffer{}
		c := cli{out: out, err: out}
		if err := c.run(args); err != nil {
			t.Fatalf("tbx %s: %v", strings.Join(args, " "), err)
		}
		text := out.String()
		for _, wanted := range []string{
			"tbx " + version.Version,
			runtime.GOOS + "/" + runtime.GOARCH,
			fmt.Sprintf("daemon protocol %d", daemon.ProtocolVersion),
		} {
			if !strings.Contains(text, wanted) {
				t.Errorf("tbx %s output missing %q:\n%s", strings.Join(args, " "), wanted, text)
			}
		}
	}
}
