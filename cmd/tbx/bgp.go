package main

import (
	"errors"
	"fmt"

	"github.com/randax/talos-box/internal/daemon"
)

func (c cli) runBGP(args []string) error {
	if len(args) != 2 || (args[0] != "enable" && args[0] != "disable") {
		return errors.New("usage: tbx bgp enable|disable <cluster>")
	}
	var result daemon.ClusterSummary
	if err := c.call("bgp."+args[0], map[string]string{"name": args[1]}, &result); err != nil {
		return err
	}
	state := "disabled"
	if result.BGP {
		state = "enabled"
	}
	_, err := fmt.Fprintf(c.out, "BGP %s for cluster %s\n", state, result.Name)
	return err
}
