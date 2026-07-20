package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
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
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if os.Geteuid() != 0 {
		return errors.New("tbx-helper must run as root")
	}
	listener, err := helper.Listen(helper.SocketPath)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(helper.SocketPath) }()

	server := helper.NewServer()
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
