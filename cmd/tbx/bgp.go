package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/randax/talos-box/internal/daemon"
)

// bgpLivenessVerbs read as "enabling BGP on demo" in both the preamble and the
// heartbeat.
var bgpLivenessVerbs = map[string]string{"enable": "enabling BGP on ", "disable": "disabling BGP on "}

func (c cli) runBGP(args []string) error {
	flags := flag.NewFlagSet("bgp", flag.ContinueOnError)
	flags.SetOutput(c.err)
	quiet := flags.Bool("quiet", false, "suppress stage narration")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 2 || (positionals[0] != "enable" && positionals[0] != "disable") {
		return errors.New(groupUsages["bgp"])
	}
	verb, name := positionals[0], positionals[1]
	// An older daemon moves the host speaker and answers in a second, leaving
	// Cilium announcing the old way — a success line for a mode that is not in
	// effect, which is exactly what #344 reported.
	if err := c.ensureBGPReconcileSupport(verb); err != nil {
		return err
	}
	var result daemon.ClusterSummary
	// The mode change re-renders Cilium, rolls its agents and applies the
	// announcement objects on this call, so it holds the same provisioning
	// budget `cluster start` does and needs the same heartbeat (#307 #344).
	signal := liveness{
		verb:     bgpLivenessVerbs[verb] + name,
		deadline: storedProvisionDeadline(name),
		quiet:    *quiet,
	}
	if err := c.callWithLivenessNarrated(signal, "bgp."+verb, map[string]string{"name": name}, &result, true); err != nil {
		return err
	}
	if err := printWarnings(c.err, result.Warnings, result.Warning); err != nil {
		return err
	}
	state := "disabled"
	if result.BGP {
		state = "enabled"
	}
	if _, err := fmt.Fprintf(c.out, "BGP %s for cluster %s\n", state, result.Name); err != nil {
		return err
	}
	if *quiet {
		return nil
	}
	for _, line := range result.Narration {
		if _, err := fmt.Fprintln(c.out, line); err != nil {
			return err
		}
	}
	return nil
}
