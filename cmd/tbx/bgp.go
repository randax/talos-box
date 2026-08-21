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
	if len(positionals) > 0 && positionals[0] != "enable" && positionals[0] != "disable" && positionals[0] != "status" {
		// A mistyped verb is named rather than answered with the bare usage, the
		// same way every other group answers one (#274).
		return unknownVerbError("bgp", positionals[0])
	}
	if len(positionals) != 2 {
		return errors.New(groupUsages["bgp"])
	}
	verb, name := positionals[0], positionals[1]
	if verb == "status" {
		return c.runBGPStatus(name)
	}
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
		subject:  name,
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
	// Only enabling has follow-up commands worth naming: the reconcile's
	// equivalent-command block is the create-time bootstrap script, and
	// replaying it after a disable read as if bootstrapping a live cluster were
	// the suggested next step (#400). Disabling is an announcement-mode flip and
	// narrates nothing beyond the stages it already printed.
	if verb == "disable" {
		return nil
	}
	for _, line := range result.Narration {
		if _, err := fmt.Fprintln(c.out, line); err != nil {
			return err
		}
	}
	return nil
}

// runBGPStatus reports the announcement mode as it stands. It exists because a
// refused or deferred mode change was otherwise only confirmable through
// `tbx doctor`, which is not where an operator looks for a BGP fact (#399).
func (c cli) runBGPStatus(name string) error {
	if err := c.ensureBGPStatusSupport(); err != nil {
		return err
	}
	var status daemon.BGPStatus
	if err := c.call("bgp.status", map[string]string{"name": name}, &status); err != nil {
		return err
	}
	if err := printWarnings(c.err, status.Warnings, ""); err != nil {
		return err
	}
	mode := "l2"
	if status.BGP {
		mode = "bgp"
	}
	if _, err := fmt.Fprintf(c.out, "cluster %s: announcement mode %s (cni: %s)\n", status.Name, mode, cniOrNone(status.CNI)); err != nil {
		return err
	}
	switch {
	case status.SpeakerError != "":
		if _, err := fmt.Fprintf(c.out, "host BGP speaker: unknown (%s)\n", status.SpeakerError); err != nil {
			return err
		}
	case status.Speaker:
		if _, err := fmt.Fprintf(c.out, "host BGP speaker: running on %s:%d\n", status.BindAddress, status.Port); err != nil {
			return err
		}
	default:
		if _, err := fmt.Fprintln(c.out, "host BGP speaker: stopped"); err != nil {
			return err
		}
	}
	if !status.Speaker {
		return nil
	}
	if len(status.Routes) == 0 {
		_, err := fmt.Fprintln(c.out, "announced routes: none")
		return err
	}
	for _, route := range status.Routes {
		if _, err := fmt.Fprintf(c.out, "announced route: %s via %s\n", route.Prefix, route.Nexthop); err != nil {
			return err
		}
	}
	return nil
}

func cniOrNone(cni string) string {
	if cni == "" {
		return "none"
	}
	return cni
}
