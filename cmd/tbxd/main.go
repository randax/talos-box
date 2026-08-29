package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/randax/talos-box/internal/balloon"
	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
	tbxdns "github.com/randax/talos-box/internal/dns"
	"github.com/randax/talos-box/internal/systemd"
	"github.com/randax/talos-box/internal/version"
)

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version.Version)
		return
	}
	if err := run(); err != nil {
		// run() has already narrated err into the daemon log; stderr and the
		// journal saw it too.
		os.Exit(1)
	}
}

func run() (err error) {
	// Own the log the runbooks point at, however tbxd was started: under
	// systemd socket activation nothing redirects stderr into it.
	if closer := startDaemonLog(); closer != nil {
		defer func() { _ = closer.Close() }()
	}
	// Narrate the terminal error while the daemon log is still open: this
	// defer runs before the closer's, so the one line explaining why tbxd
	// exited reaches tbxd.log and not just the journal.
	defer func() {
		if err != nil {
			log.Print(err)
		}
	}()
	// Keep client-library chatter out of the log the runbooks point at (#401).
	if closer := startKubernetesLogRouting(log.Printf); closer != nil {
		defer func() { _ = closer.Close() }()
	}
	socketPath, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	listener, activated, err := systemd.InheritedListener(socketPath)
	if err != nil {
		return err
	}
	if !activated {
		listener, err = daemon.Listen(socketPath)
	}
	if err != nil {
		return err
	}
	if !activated {
		defer func() { _ = os.Remove(socketPath) }()
	}

	server, err := daemon.NewServer(context.Background())
	if err != nil {
		_ = listener.Close()
		return err
	}
	// The authority predicate runs on every DNS query; it answers from an
	// in-memory snapshot refreshed in the background, so query latency never
	// depends on state-directory IO.
	domainSource := newClusterDomainSource(cluster.List)
	domainSource.refresh()
	stopDomainRefresh := domainSource.refreshEvery(domainRefreshEvery)
	defer stopDomainRefresh()
	dnsService, err := startDNSService(func(name string) net.IP {
		clusters, err := cluster.List()
		if err != nil {
			log.Printf("DNS state refresh failed: %v", err)
			return nil
		}
		return tbxdns.Resolve(name, clusters, cluster.LookupIP)
	}, tbxdns.Authority(domainSource.snapshot))
	if err != nil {
		_ = listener.Close()
		return err
	}
	configureHostNetworking()
	// The helper holds tbxd's copy of the cluster reservations; a helper that
	// restarted (or has never been synced) serves none until this lands.
	if err := daemon.SyncHelperState(); err != nil {
		log.Printf("startup helper state sync: %v", err)
	}
	stopHostNetworkingMaintenance := startHostNetworkingMaintenance()
	// registry mirrors are bound per cluster gateway by the daemon (see #39).

	balloonStop := make(chan struct{})
	balloonConfig := balloon.DefaultConfig()
	// Hold the pre-balloon the provision-start gate takes for a booting guest,
	// so the manager does not hand it straight back (#398).
	balloonConfig.HoldMiB = server.BalloonHoldMiB
	// skip ticks while VMs are being torn down (#513)
	balloonConfig.Paused = server.BalloonPaused
	go balloon.Run(balloonConfig, server.Balloonables, balloonStop)
	defer close(balloonStop)

	serveErrors := make(chan error, 2)
	go func() { serveErrors <- server.Serve(listener) }()

	signal.Ignore(os.Interrupt)
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	defer signal.Stop(terminated)

	select {
	case err := <-serveErrors:
		stopHostNetworkingMaintenance()
		shutdownErr := errors.Join(server.Shutdown(), dnsService.Close())
		return errors.Join(err, shutdownErr)
	case err := <-dnsService.Errors():
		stopHostNetworkingMaintenance()
		shutdownErr := errors.Join(server.Shutdown(), dnsService.Close())
		return errors.Join(err, <-serveErrors, shutdownErr)
	case <-terminated:
		stopHostNetworkingMaintenance()
		shutdownErr := errors.Join(server.Shutdown(), dnsService.Close())
		return errors.Join(shutdownErr, <-serveErrors)
	}
}
