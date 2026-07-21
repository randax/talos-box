package main

import (
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
	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/version"
	"github.com/randax/talos-box/internal/vm"
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
	listener, err := daemon.Listen(socketPath)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(socketPath) }()

	server, err := daemon.NewServer()
	if err != nil {
		_ = listener.Close()
		return err
	}
	dnsServer, err := tbxdns.Listen(tbxdns.Address, func(name string) net.IP {
		clusters, err := cluster.List()
		if err != nil {
			log.Printf("DNS state refresh failed: %v", err)
			return nil
		}
		return tbxdns.Resolve(name, clusters, vm.LeaseIP)
	})
	if err != nil {
		_ = listener.Close()
		return err
	}
	configureHostNetworking()
	stopHostNetworkingMaintenance := startHostNetworkingMaintenance()
	// registry mirrors are bound per cluster gateway by the daemon (see #39).

	balloonStop := make(chan struct{})
	go balloon.Run(balloon.DefaultConfig(), server.Balloonables, balloonStop)
	defer close(balloonStop)

	serveErrors := make(chan error, 2)
	go func() { serveErrors <- server.Serve(listener) }()
	go func() { serveErrors <- dnsServer.Serve() }()

	signal.Ignore(os.Interrupt)
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	defer signal.Stop(terminated)

	select {
	case err := <-serveErrors:
		stopHostNetworkingMaintenance()
		shutdownErr := errors.Join(server.Shutdown(), dnsServer.Close())
		return errors.Join(err, <-serveErrors, shutdownErr)
	case <-terminated:
		stopHostNetworkingMaintenance()
		shutdownErr := errors.Join(server.Shutdown(), dnsServer.Close())
		return errors.Join(shutdownErr, <-serveErrors, <-serveErrors)
	}
}

func configureHostNetworking() {
	client, err := helper.Connect()
	if err != nil {
		log.Printf("network helper unavailable; run `sudo tbx system install`: %v", err)
		return
	}
	defer func() { _ = client.Close() }()
	if err := client.InstallDNS(tbxdns.Port); err != nil {
		log.Printf("install DNS resolver: %v", err)
	}
	if err := client.EnableForwarding(); err != nil {
		log.Printf("enable IP forwarding: %v", err)
	}
}
