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
	"github.com/randax/talos-box/internal/daemon"
	tbxdns "github.com/randax/talos-box/internal/dns"
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/resolverset"
)

const (
	resolverPath             = resolverset.SharedPath
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
	EnableForwarding() error
	Close() error
}

func configureHostNetworking() {
	client, err := helper.Connect()
	if err != nil {
		log.Printf("network helper unavailable; %s: %v", helper.UnavailableAdvice(), err)
		return
	}
	defer func() { _ = client.Close() }()
	if err := client.InstallDNS(tbxdns.Port); err != nil {
		log.Printf("install DNS resolver: %v", err)
	}
	if err := client.EnableForwarding(); err != nil {
		log.Printf("enable IP forwarding: %v", err)
	}
	// Resolver-file mutation has a single serialized owner so this can never
	// race the create/destroy paths with a stale domain set.
	if err := daemon.SyncResolverFiles(); err != nil {
		log.Printf("startup resolver-file sync: %v", err)
	}
}

// customClusterDomains lists the explicit domains of live clusters; clusters
// on the default domain are served by the shared resolver file. The error
// matters: an unreadable state must never be treated as "no custom domains",
// or the reconciler would delete every live custom-domain resolver file.
func customClusterDomains() ([]string, error) {
	clusters, err := cluster.List()
	if err != nil {
		return nil, fmt.Errorf("load clusters for resolver maintenance: %w", err)
	}
	var domains []string
	for _, item := range clusters {
		if item.Domain != "" {
			domains = append(domains, item.Domain)
		}
	}
	return domains, nil
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
		maintainHostNetworking(stop, ticker.C, customClusterDomains, os.ReadFile, listResolverFiles, runHostCommand, daemon.SyncResolverFiles, connectHostNetworkingHelper)
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
	customDomains func() ([]string, error),
	readFile func(string) ([]byte, error),
	listResolvers func() (map[string][]byte, error),
	run func(string, ...string) ([]byte, error),
	syncDomains func() error,
	connect func() (hostNetworkingClient, error),
) {
	for {
		select {
		case <-stop:
			return
		case <-ticks:
			domains, domainsErr := customDomains()
			drift := checkHostNetworking(tbxdns.Port, domains, readFile, listResolvers, run)
			if domainsErr != nil {
				// Fail closed on the domain set only: without trustworthy
				// state, reconciling would treat live custom domains as
				// orphans and delete their files. Shared-resolver and
				// forwarding repair are state-independent and continue.
				log.Printf("skip custom-domain resolver check: %v", domainsErr)
				drift.domains = false
			}
			if !drift.any() {
				continue
			}
			if err := reassertHostNetworking(drift, syncDomains, connect); err != nil {
				log.Printf("host networking drift detected (%s); re-assertion failed: %v", drift.description(), err)
				continue
			}
			log.Printf("host networking drift detected (%s); re-asserted", drift.description())
		}
	}
}

func reassertHostNetworking(drift hostNetworkingDrift, syncDomains func() error, connect func() (hostNetworkingClient, error)) error {
	var repairErr error
	if drift.domains {
		// The repair re-reads state under the daemon's resolver-sync lock,
		// so a domain set observed before a concurrent create/destroy is
		// never applied.
		repairErr = errors.Join(repairErr, syncDomains())
	}
	if !drift.dns && !drift.forwarding {
		return repairErr
	}
	client, err := connect()
	if err != nil {
		return errors.Join(repairErr, fmt.Errorf("connect to helper: %w", err))
	}
	defer func() { _ = client.Close() }()
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
