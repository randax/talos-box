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
		log.Fatal(err)
	}
}

func run() error {
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
	stopHostNetworkingMaintenance := startHostNetworkingMaintenance()
	// registry mirrors are bound per cluster gateway by the daemon (see #39).

	balloonStop := make(chan struct{})
	balloonConfig := balloon.DefaultConfig()
	// Hold the pre-balloon the provision-start gate takes for a booting guest,
	// so the manager does not hand it straight back (#398).
	balloonConfig.HoldMiB = server.BalloonHoldMiB
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
