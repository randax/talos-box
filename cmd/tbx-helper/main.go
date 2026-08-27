package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
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
	state := loadHelperState()
	// Startup convergence is best-effort: a failure here is logged and
	// reported again, with its remedy, to the next net.sync. Exiting instead
	// would trip the unit's start limit and take the socket down with it —
	// the crash loop #467 describes — for a condition that needs the operator,
	// not a restart.
	if err := helper.ConvergeNetworking(state.SubnetIndexes()); err != nil {
		log.Printf("converge helper networking at startup: %v", err)
	}

	server := helper.NewServer(state, serverAllowedUID(allowedUID, explicitAllowedUID, activated), activated)
	if err := server.ConvergeServices(); err != nil {
		log.Printf("converge helper services at startup: %v", err)
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

// loadHelperState reads the reservations tbxd last pushed, so a restarted
// helper reconverges host networking without waiting for a daemon. Without a
// state directory the helper keeps them in memory only: it still serves, but a
// restart forgets them until tbxd syncs again.
func loadHelperState() *helper.State {
	directory := helper.StateDir()
	if directory == "" && runtime.GOOS == "linux" {
		log.Print("no helper state directory (StateDirectory= or TBX_HELPER_STATE_DIR); reservations are in-memory only")
	}
	state := helper.NewState(directory)
	// Load never fails: an unusable file is logged and treated as empty.
	_ = state.Load()
	return state
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
