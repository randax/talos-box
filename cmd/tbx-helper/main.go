package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/randax/talos-box/internal/helper"
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
	if os.Geteuid() != 0 {
		return errors.New("tbx-helper must run as root")
	}
	if allowedUID == nil {
		log.Print("warning: --allowed-uid is not configured; only root can use tbx-helper; re-run `sudo tbx system install` from your account")
	}
	listener, err := helper.Listen(helper.SocketPath)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(helper.SocketPath) }()

	server := helper.NewServer(allowedUID)
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

func parseAllowedUID(args []string) (*uint32, error) {
	flags := flag.NewFlagSet("tbx-helper", flag.ContinueOnError)
	var allowedUID *uint32
	flags.Func("allowed-uid", "UID authorized to use the helper", func(value string) error {
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
