//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	tbxdns "github.com/randax/talos-box/internal/dns"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/resolverset"
)

const (
	resolverPath             = "/etc/resolver/k8s.test"
	hostNetworkingCheckEvery = 60 * time.Second
	hostCommandTimeout       = 10 * time.Second
)

type hostNetworkingDrift struct {
	dns        bool
	domains    bool
	forwarding bool
}

type hostNetworkingClient interface {
	InstallDNS(port int) error
	SyncDomainResolvers(domains []string, port int) error
	EnableForwarding() error
	Close() error
}

func configureHostNetworking() {
	client, err := helper.Connect()
	if err != nil {
		log.Printf("network helper unavailable; run sudo tbx system install: %v", err)
		return
	}
	defer func() { _ = client.Close() }()
	if err := client.InstallDNS(tbxdns.Port); err != nil {
		log.Printf("install DNS resolver: %v", err)
	}
	if err := client.SyncDomainResolvers(customClusterDomains(), tbxdns.Port); err != nil {
		log.Printf("sync custom-domain resolvers: %v", err)
	}
	if err := client.EnableForwarding(); err != nil {
		log.Printf("enable IP forwarding: %v", err)
	}
}

// customClusterDomains lists the explicit domains of live clusters; clusters
// on the default domain are served by the shared resolver file.
func customClusterDomains() []string {
	clusters, err := cluster.List()
	if err != nil {
		log.Printf("load clusters for resolver maintenance: %v", err)
		return nil
	}
	var domains []string
	for _, item := range clusters {
		if item.Domain != "" {
			domains = append(domains, item.Domain)
		}
	}
	return domains
}

// listResolverFiles reads /etc/resolver as name → content; observation only,
// mutations stay behind the privileged helper.
func listResolverFiles() (map[string][]byte, error) {
	directory := filepath.Dir(resolverPath)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	observed := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		observed[entry.Name()] = content
	}
	return observed, nil
}

// checkHostNetworking only observes unprivileged host state. Its inputs are
// injected so the drift classification remains deterministic in tests.
func checkHostNetworking(
	port int,
	customDomains []string,
	readFile func(string) ([]byte, error),
	listResolvers func() (map[string][]byte, error),
	run func(string, ...string) ([]byte, error),
) hostNetworkingDrift {
	wantResolver := fmt.Sprintf("nameserver 127.0.0.1\nport %d\n", port)
	resolver, err := readFile(resolverPath)
	dnsDrifted := err != nil || string(resolver) != wantResolver

	observed, err := listResolvers()
	domainsDrifted := err != nil
	if err == nil {
		delete(observed, filepath.Base(resolverPath))
		create, remove := resolverset.Plan(customDomains, observed, port)
		domainsDrifted = len(create) != 0 || len(remove) != 0
	}

	forwarding, err := run("/usr/sbin/sysctl", "-n", "net.inet.ip.forwarding")
	forwardingDrifted := err != nil || strings.TrimSpace(string(forwarding)) != "1"

	return hostNetworkingDrift{dns: dnsDrifted, domains: domainsDrifted, forwarding: forwardingDrifted}
}

func (d hostNetworkingDrift) any() bool {
	return d.dns || d.domains || d.forwarding
}

func (d hostNetworkingDrift) description() string {
	var names []string
	if d.dns {
		names = append(names, "DNS resolver")
	}
	if d.domains {
		names = append(names, "custom-domain resolvers")
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
		maintainHostNetworking(stop, ticker.C, customClusterDomains, os.ReadFile, listResolverFiles, runHostCommand, connectHostNetworkingHelper)
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
	customDomains func() []string,
	readFile func(string) ([]byte, error),
	listResolvers func() (map[string][]byte, error),
	run func(string, ...string) ([]byte, error),
	connect func() (hostNetworkingClient, error),
) {
	for {
		select {
		case <-stop:
			return
		case <-ticks:
			domains := customDomains()
			drift := checkHostNetworking(tbxdns.Port, domains, readFile, listResolvers, run)
			if !drift.any() {
				continue
			}
			if err := reassertHostNetworking(drift, domains, connect); err != nil {
				log.Printf("host networking drift detected (%s); re-assertion failed: %v", drift.description(), err)
				continue
			}
			log.Printf("host networking drift detected (%s); re-asserted", drift.description())
		}
	}
}

func reassertHostNetworking(drift hostNetworkingDrift, customDomains []string, connect func() (hostNetworkingClient, error)) error {
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
	if drift.domains {
		if err := client.SyncDomainResolvers(customDomains, tbxdns.Port); err != nil {
			repairErr = errors.Join(repairErr, fmt.Errorf("sync custom-domain resolvers: %w", err))
		}
	}
	if drift.forwarding {
		if err := client.EnableForwarding(); err != nil {
			repairErr = errors.Join(repairErr, fmt.Errorf("enable IP forwarding: %w", err))
		}
	}
	return repairErr
}

// runHostCommand is bounded so a stuck utility cannot wedge the maintenance
// loop, whose stop function waits for the loop to exit during shutdown.
func runHostCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hostCommandTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).Output()
	if ctx.Err() != nil {
		return output, fmt.Errorf("%s timed out after %s", name, hostCommandTimeout)
	}
	return output, err
}

func connectHostNetworkingHelper() (hostNetworkingClient, error) {
	return helper.Connect()
}
