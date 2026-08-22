//go:build linux

package helper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
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

func platformRegisterDNS(clusterDomain string, subnetIndex int) DNSRegistration {
	registration := applyResolvedRegistration(clusterDomain, subnetIndex, runResolvedCommand)
	if !registration.Registered {
		_ = runResolvedCommand("resolvectl", "revert", bridgeNameForSubnet(subnetIndex))
	}
	return registration
}

func platformUnregisterDNS(subnetIndex int) error {
	return revertResolvedLink(subnetIndex, runResolvedCommand)
}

func revertResolvedLink(subnetIndex int, run dnsCommandRunner) error {
	err := run("resolvectl", "revert", bridgeNameForSubnet(subnetIndex))
	// The bridge is removed with the cluster that owned it (#445), so the
	// daemon's DNS reconciler routinely withdraws a registration whose link is
	// already gone. Nothing can stay registered against a link that does not
	// exist, so already gone is success — the same rule DeleteBridge follows.
	if errors.Is(err, exec.ErrNotFound) || resolvedLinkAbsent(err) {
		return nil
	}
	return err
}

// resolvedLinkAbsent reports whether a resolvectl failure was the link itself
// being gone rather than resolved refusing the change. resolvectl resolves the
// interface name before it talks to resolved and prints the kernel's own
// wording — `Failed to resolve interface "br-tbx0": No such device` — which
// runResolvedCommand keeps in the error it returns.
func resolvedLinkAbsent(err error) bool {
	if err == nil {
		return false
	}
	lowered := strings.ToLower(err.Error())
	for _, absent := range []string{"no such device", "unknown interface", "cannot find device", "link not found"} {
		if strings.Contains(lowered, absent) {
			return true
		}
	}
	return false
}

func runResolvedCommand(name string, args ...string) error {
	// resolvectl is systemd's supported D-Bus client. Keeping that protocol
	// boundary in the helper exercises resolved's polkit authorization without
	// adding a second D-Bus implementation dependency to talos-box.
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
