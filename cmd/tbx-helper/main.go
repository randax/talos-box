package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/randax/talos-box/internal/helper"
	"github.com/randax/talos-box/internal/systemd"
	"github.com/randax/talos-box/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version.Version)
		return
	}

	log.SetFlags(log.LstdFlags)
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	allowedUID, err := parseAllowedUID(args)
	if err != nil {
		return err
	}
	explicitAllowedUID := allowedUID != nil
	allowedUID, err = resolveAllowedUID(allowedUID)
	if err != nil {
		return err
	}
	if err := requirePrivileges(); err != nil {
		return err
	}
	warnMissingAllowedUID(allowedUID)
	listener, socketPath, activated, err := openHelperListener(
		systemd.InheritedListener,
		func() (string, error) { return helper.ServerSocketPath(allowedUID) },
		helper.Listen,
	)
	if err != nil {
		return err
	}
	if !activated {
		defer func() { _ = os.Remove(socketPath) }()
	}
	if err := helper.ConvergeNetworking(); err != nil {
		_ = listener.Close()
		return fmt.Errorf("converge helper networking: %w", err)
	}

	server := helper.NewServer(serverAllowedUID(allowedUID, explicitAllowedUID, activated), activated)
	if err := server.ConvergeServices(); err != nil {
		_ = listener.Close()
		return fmt.Errorf("converge helper services: %w", err)
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()

	signal.Ignore(os.Interrupt)
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	defer signal.Stop(terminated)

	select {
	case err := <-serveErrors:
		return errors.Join(err, server.Shutdown())
	case <-terminated:
		shutdownErr := server.Shutdown()
		serveErr := <-serveErrors
		return errors.Join(shutdownErr, serveErr)
	}
}

// activatedListenerName labels the inherited descriptor. Under socket
// activation the descriptor is the address, so no path is resolved: the
// packaged unit runs as an unprivileged user whose runtime directory does not
// hold the socket the unit listens on, and resolving it there fails.
const activatedListenerName = "tbx-helper.sock"

// openHelperListener prefers a systemd-activated descriptor and only resolves
// and binds a socket path when there is none. The returned path is empty when
// activated: nothing on the activated path owns the socket file.
func openHelperListener(
	inherited func(string) (net.Listener, bool, error),
	resolve func() (string, error),
	listen func(string) (net.Listener, error),
) (net.Listener, string, bool, error) {
	listener, activated, err := inherited(activatedListenerName)
	if err != nil {
		return nil, "", false, err
	}
	if activated {
		return listener, "", true, nil
	}
	socketPath, err := resolve()
	if err != nil {
		return nil, "", false, fmt.Errorf("resolve helper socket path: %w", err)
	}
	listener, err = listen(socketPath)
	if err != nil {
		return nil, "", false, err
	}
	return listener, socketPath, false, nil
}

func serverAllowedUID(resolved *uint32, explicit, activated bool) *uint32 {
	if activated && !explicit {
		return nil
	}
	return resolved
}

func parseAllowedUID(args []string) (*uint32, error) {
	flags := flag.NewFlagSet("tbx-helper", flag.ContinueOnError)
	var allowedUID *uint32
	flags.Func("allowed-uid", "UID authorized to use the helper (Linux defaults to SUDO_UID or the helper UID)", func(value string) error {
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return fmt.Errorf("invalid uid %q: %w", value, err)
		}
		uid := uint32(parsed)
		allowedUID = &uid
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return allowedUID, nil
}
