package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/randax/talos-box/internal/daemon"
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
