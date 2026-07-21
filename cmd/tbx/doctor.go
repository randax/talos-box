package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/randax/talos-box/internal/daemon"
	tbxdns "github.com/randax/talos-box/internal/dns"
	"github.com/randax/talos-box/internal/helper"
)

const resolverPath = "/etc/resolver/k8s.test"

type doctorDependencies struct {
	checkHelper     func() error
	checkResolver   func() error
	checkDirectDNS  func() error
	checkForwarding func() error
	listClusters    func() ([]daemon.ClusterSummary, error)
	getStatus       func() ([]daemon.ClusterStatus, error)
	command         commandOutput
	doHTTP          httpDo
}

type doctorFinding struct {
	level  string
	check  string
	detail string
}

func (c cli) runDoctor(args []string) error {
	return c.runDoctorWithDependencies(args, c.doctorDependencies())
}

func (c cli) runDoctorWithDependencies(args []string, deps doctorDependencies) error {
	if len(args) != 0 {
		return errors.New("usage: tbx doctor")
	}

	failed := false
	writeFindings := func(findings ...doctorFinding) error {
		for _, finding := range findings {
			if finding.level == "FAIL" {
				failed = true
			}
			if finding.detail == "" {
				if _, err := fmt.Fprintf(c.out, "%s %s\n", finding.level, finding.check); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(c.out, "%s %s: %s\n", finding.level, finding.check, finding.detail); err != nil {
				return err
			}
		}
		return nil
	}

	for _, check := range []struct {
		name string
		run  func() error
	}{
		{name: "helper", run: deps.checkHelper},
		{name: "resolver", run: deps.checkResolver},
		{name: "DNS", run: deps.checkDirectDNS},
		{name: "forwarding", run: deps.checkForwarding},
	} {
		finding := doctorFinding{level: "PASS", check: check.name}
		if err := check.run(); err != nil {
			finding.level, finding.detail = "FAIL", err.Error()
		}
		if err := writeFindings(finding); err != nil {
			return err
		}
	}

	clusters, clusterErr := deps.listClusters()
	if isDaemonUnavailable(clusterErr) {
		detail := fmt.Sprintf("daemon unavailable: %v", clusterErr)
		if err := writeFindings(
			doctorFinding{level: "SKIP", check: "system-dns", detail: detail},
			doctorFinding{level: "SKIP", check: "routes", detail: detail},
		); err != nil {
			return err
		}
	} else if clusterErr != nil {
		detail := fmt.Sprintf("list clusters: %v", clusterErr)
		if err := writeFindings(
			doctorFinding{level: "FAIL", check: "system-dns", detail: detail},
			doctorFinding{level: "FAIL", check: "routes", detail: detail},
		); err != nil {
			return err
		}
	} else if len(clusters) == 0 {
		if err := writeFindings(
			doctorFinding{level: "SKIP", check: "system-dns", detail: "no clusters exist"},
			doctorFinding{level: "SKIP", check: "routes", detail: "no clusters exist"},
		); err != nil {
			return err
		}
	} else {
		dnsFinding := doctorFinding{level: "PASS", check: "system-dns"}
		if err := checkSystemDNS(clusters, deps.command); err != nil {
			dnsFinding.level, dnsFinding.detail = "FAIL", err.Error()
		}
		if err := writeFindings(dnsFinding); err != nil {
			return err
		}

		statuses, statusErr := deps.getStatus()
		routeFinding := doctorFinding{level: "PASS", check: "routes"}
		var routeProblems []string
		if statusErr != nil {
			routeProblems = append(routeProblems,
				fmt.Sprintf("cluster status unavailable; node routes could not be checked: %v", statusErr))
		}
		if err := checkClusterRoutes(clusters, statuses, deps.command); err != nil {
			routeProblems = append(routeProblems, err.Error())
		}
		if len(routeProblems) != 0 {
			routeFinding.level, routeFinding.detail = "FAIL", strings.Join(routeProblems, "; ")
		}
		if err := writeFindings(routeFinding); err != nil {
			return err
		}
	}

	if err := writeFindings(egressFinding(probeFactoryEgress(deps.doHTTP))); err != nil {
		return err
	}
	if err := writeFindings(securityInventoryFindings(deps.command)...); err != nil {
		return err
	}

	if failed {
		return errors.New("one or more doctor checks failed")
	}
	return nil
}

func isDaemonUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var connectionError dialError
	return errors.As(err, &connectionError)
}

func (c cli) doctorDependencies() doctorDependencies {
	command := execCombinedOutput
	return doctorDependencies{
		checkHelper:     checkHelper,
		checkResolver:   checkResolver,
		checkDirectDNS:  func() error { return tbxdns.Probe(tbxdns.Address) },
		checkForwarding: checkForwarding,
		listClusters: func() ([]daemon.ClusterSummary, error) {
			var result []daemon.ClusterSummary
			err := c.doctorCall("cluster.list", struct{}{}, &result)
			return result, err
		},
		getStatus: func() ([]daemon.ClusterStatus, error) {
			var result []daemon.ClusterStatus
			err := c.doctorCall("status", map[string]string{"cluster": ""}, &result)
			return result, err
		},
		command: command,
		doHTTP:  newDoctorHTTPClient().Do,
	}
}

// doctorCall deliberately uses exchange directly instead of cli.call: diagnostics
// must report an absent daemon as SKIP, not start one as a side effect.
func (c cli) doctorCall(op string, args, destination any) error {
	socketPath, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	response, err := exchange(socketPath, op, args)
	if err != nil {
		return err
	}
	if !response.OK {
		if response.Error == "" {
			return errors.New("daemon operation failed")
		}
		return errors.New(response.Error)
	}
	if destination != nil && len(response.Data) > 0 {
		if err := json.Unmarshal(response.Data, destination); err != nil {
			return fmt.Errorf("decode daemon result: %w", err)
		}
	}
	return nil
}

func checkHelper() error {
	client, err := helper.Connect()
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return client.Ping()
}

func checkResolver() error {
	info, err := os.Stat(resolverPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("resolver path is not a regular file")
	}
	return nil
}

func checkForwarding() error {
	output, err := exec.Command("/usr/sbin/sysctl", "-n", "net.inet.ip.forwarding").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) != "1" {
		return fmt.Errorf("net.inet.ip.forwarding is %q, want 1", strings.TrimSpace(string(output)))
	}
	return nil
}
