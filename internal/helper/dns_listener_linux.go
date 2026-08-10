//go:build linux

package helper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/randax/talos-box/internal/cluster"
)

const resolvedCommandTimeout = 10 * time.Second

func platformBindDNS(subnetIndex int) (*os.File, error) {
	if err := convergeNetworking(); err != nil {
		return nil, fmt.Errorf("converge host networking before DNS bind: %w", err)
	}
	address := &net.UDPAddr{IP: net.ParseIP(cluster.Gateway(subnetIndex)).To4(), Port: 53}
	connection, err := net.ListenUDP("udp4", address)
	if err != nil {
		return nil, fmt.Errorf("bind cluster DNS on %s: %w", address, err)
	}
	file, err := connection.File()
	closeErr := connection.Close()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("duplicate cluster DNS socket: %w", err), closeErr)
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("release helper DNS socket copy: %w", closeErr)
	}
	return file, nil
}

func platformRegisterDNS(clusterName string, subnetIndex int) DNSRegistration {
	registration := applyResolvedRegistration(clusterName, subnetIndex, runResolvedCommand)
	if !registration.Registered {
		_ = runResolvedCommand("resolvectl", "revert", bridgeNameForSubnet(subnetIndex))
	}
	return registration
}

func platformUnregisterDNS(subnetIndex int) error {
	err := runResolvedCommand("resolvectl", "revert", bridgeNameForSubnet(subnetIndex))
	if errors.Is(err, exec.ErrNotFound) {
		return nil
	}
	return err
}

func runResolvedCommand(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), resolvedCommandTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("%s timed out after %s", name, resolvedCommandTimeout)
	}
	if err != nil {
		detail := string(output)
		if detail == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, detail)
	}
	return nil
}
