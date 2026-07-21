package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	tbxdns "github.com/randax/talos-box/internal/dns"
	"github.com/randax/talos-box/internal/helper"
)

const (
	resolverPath             = "/etc/resolver/k8s.test"
	hostNetworkingCheckEvery = 60 * time.Second
)

type hostNetworkingDrift struct {
	dns        bool
	forwarding bool
}

type hostNetworkingClient interface {
	InstallDNS(port int) error
	EnableForwarding() error
	Close() error
}

// checkHostNetworking only observes unprivileged host state. Its inputs are
// injected so the drift classification remains deterministic in tests.
func checkHostNetworking(
	port int,
	readFile func(string) ([]byte, error),
	run func(string, ...string) ([]byte, error),
) hostNetworkingDrift {
	wantResolver := fmt.Sprintf("nameserver 127.0.0.1\nport %d\n", port)
	resolver, err := readFile(resolverPath)
	dnsDrifted := err != nil || string(resolver) != wantResolver

	forwarding, err := run("/usr/sbin/sysctl", "-n", "net.inet.ip.forwarding")
	forwardingDrifted := err != nil || strings.TrimSpace(string(forwarding)) != "1"

	return hostNetworkingDrift{dns: dnsDrifted, forwarding: forwardingDrifted}
}

func (d hostNetworkingDrift) any() bool {
	return d.dns || d.forwarding
}

func (d hostNetworkingDrift) description() string {
	var names []string
	if d.dns {
		names = append(names, "DNS resolver")
	}
	if d.forwarding {
		names = append(names, "IP forwarding")
	}
	return strings.Join(names, ", ")
}

func startHostNetworkingMaintenance() func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	ticker := time.NewTicker(hostNetworkingCheckEvery)
	go func() {
		defer close(done)
		maintainHostNetworking(stop, ticker.C, os.ReadFile, runHostCommand, connectHostNetworkingHelper)
	}()
	return func() {
		ticker.Stop()
		close(stop)
		<-done
	}
}

func maintainHostNetworking(
	stop <-chan struct{},
	ticks <-chan time.Time,
	readFile func(string) ([]byte, error),
	run func(string, ...string) ([]byte, error),
	connect func() (hostNetworkingClient, error),
) {
	for {
		select {
		case <-stop:
			return
		case <-ticks:
			drift := checkHostNetworking(tbxdns.Port, readFile, run)
			if !drift.any() {
				continue
			}
			if err := reassertHostNetworking(drift, connect); err != nil {
				log.Printf("host networking drift detected (%s); re-assertion failed: %v", drift.description(), err)
				continue
			}
			log.Printf("host networking drift detected (%s); re-asserted", drift.description())
		}
	}
}

func reassertHostNetworking(drift hostNetworkingDrift, connect func() (hostNetworkingClient, error)) error {
	client, err := connect()
	if err != nil {
		return fmt.Errorf("connect to helper: %w", err)
	}
	defer func() { _ = client.Close() }()

	var repairErr error
	if drift.dns {
		if err := client.InstallDNS(tbxdns.Port); err != nil {
			repairErr = errors.Join(repairErr, fmt.Errorf("install DNS resolver: %w", err))
		}
	}
	if drift.forwarding {
		if err := client.EnableForwarding(); err != nil {
			repairErr = errors.Join(repairErr, fmt.Errorf("enable IP forwarding: %w", err))
		}
	}
	return repairErr
}

func runHostCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

func connectHostNetworkingHelper() (hostNetworkingClient, error) {
	return helper.Connect()
}
