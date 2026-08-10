package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/hostpressure"
)

type doctorDependencies struct {
	checkHelper     func() error
	checkResolver   func() error
	checkDirectDNS  func() error
	checkForwarding func() error
	listClusters    func() ([]daemon.ClusterSummary, error)
	listConfig      func() ([]cluster.Cluster, error)
	getStatus       func() ([]daemon.ClusterStatus, error)
	hostPressure    func() (hostpressure.Snapshot, error)
	command         commandOutput
	readFile        func(string) ([]byte, error)
	accessRW        func(string) error
	listenPacket    func(string, string) (net.PacketConn, error)
	listenStream    func(string, string) (io.Closer, error)
	doHTTP          httpDo
	platform        func() []doctorFinding
}

type doctorFinding struct {
	level  string
	check  string
	detail string
}

type skippedDoctorCheck struct{ detail string }

func (e skippedDoctorCheck) Error() string { return e.detail }

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
			var skipped skippedDoctorCheck
			if errors.As(err, &skipped) {
				finding.level, finding.detail = "SKIP", skipped.Error()
			} else if check.name == "resolver" {
				finding.level, finding.detail = classifyResolverFailure(err)
			} else {
				finding.level, finding.detail = "FAIL", err.Error()
			}
		}
		if err := writeFindings(finding); err != nil {
			return err
		}
	}
	if deps.platform != nil {
		if err := writeFindings(deps.platform()...); err != nil {
			return err
		}
	}

	if deps.hostPressure == nil {
		if err := writeFindings(doctorFinding{level: "SKIP", check: "host-pressure", detail: "probe unavailable"}); err != nil {
			return err
		}
	} else if snapshot, err := deps.hostPressure(); err != nil {
		if err := writeFindings(doctorFinding{level: "SKIP", check: "host-pressure", detail: err.Error()}); err != nil {
			return err
		}
	} else if warnings := hostpressure.Warnings(snapshot); len(warnings) == 0 {
		if err := writeFindings(doctorFinding{level: "PASS", check: "host-pressure"}); err != nil {
			return err
		}
	} else {
		for _, warning := range warnings {
			if err := writeFindings(doctorFinding{level: "WARN", check: "host-pressure", detail: warning}); err != nil {
				return err
			}
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
			dnsFinding.level, dnsFinding.detail = classifySystemDNSFailure(err)
		}
		if err := writeFindings(dnsFinding); err != nil {
			return err
		}

		anyRunning := false
		for _, item := range clusters {
			if item.Running {
				anyRunning = true
				break
			}
		}
		if !anyRunning {
			// stopped clusters have no interfaces to route through
			if err := writeFindings(doctorFinding{level: "SKIP", check: "routes", detail: "no clusters are running"}); err != nil {
				return err
			}
		} else {
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
	deps := doctorDependencies{
		checkHelper:     checkHelper,
		checkResolver:   checkResolver,
		checkDirectDNS:  checkPlatformDirectDNS,
		checkForwarding: checkForwarding,
		listConfig:      cluster.List,
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
		hostPressure: func() (hostpressure.Snapshot, error) {
			home, err := os.UserHomeDir()
			if err != nil {
				return hostpressure.Snapshot{}, fmt.Errorf("find home directory: %w", err)
			}
			return hostpressure.SystemSnapshot(filepath.Join(home, ".talosbox"))
		},
		command:  command,
		readFile: os.ReadFile,
		accessRW: func(path string) error {
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return file.Close()
		},
		listenPacket: func(network, address string) (net.PacketConn, error) {
			return net.ListenPacket(network, address)
		},
		listenStream: func(network, address string) (io.Closer, error) {
			return net.Listen(network, address)
		},
		doHTTP: newDoctorHTTPClient().Do,
	}
	platformDoctorDependencies(&deps)
	return deps
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
