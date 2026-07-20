package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/randax/talos-box/internal/daemon"
)

type dialError struct{ err error }

func (e dialError) Error() string { return e.err.Error() }

func (c cli) call(op string, args, destination any) error {
	socketPath, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	response, err := exchange(socketPath, op, args)
	var connectionError dialError
	if errors.As(err, &connectionError) {
		if err := startDaemon(); err != nil {
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		backoff := 50 * time.Millisecond
		for {
			response, err = exchange(socketPath, op, args)
			if !errors.As(err, &connectionError) || time.Now().After(deadline) {
				break
			}
			time.Sleep(backoff)
			if backoff < 500*time.Millisecond {
				backoff *= 2
			}
		}
	}
	if err != nil {
		return err
	}
	if !response.OK {
		if response.Error == "" {
			return errors.New("daemon operation failed")
		}
		return errors.New(response.Error)
	}
	if destination != nil && len(response.Data) > 0 {
		if err := json.Unmarshal(response.Data, destination); err != nil {
			return fmt.Errorf("decode daemon result: %w", err)
		}
	}
	return nil
}

func exchange(socketPath, op string, args any) (daemon.Response, error) {
	connection, err := net.DialTimeout("unix", socketPath, 250*time.Millisecond)
	if err != nil {
		return daemon.Response{}, dialError{err: err}
	}
	defer func() { _ = connection.Close() }()

	rawArgs, err := json.Marshal(args)
	if err != nil {
		return daemon.Response{}, fmt.Errorf("encode request arguments: %w", err)
	}
	request := daemon.Request{Op: op, Args: rawArgs}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return daemon.Response{}, fmt.Errorf("write daemon request: %w", err)
	}
	var response daemon.Response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return daemon.Response{}, fmt.Errorf("read daemon response: %w", err)
	}
	return response, nil
}

func startDaemon() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find tbx executable: %w", err)
	}
	daemonPath := filepath.Join(filepath.Dir(executable), "tbxd")
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}
	stateDir := filepath.Join(home, ".talosbox")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create daemon directory: %w", err)
	}
	logPath := filepath.Join(stateDir, "tbxd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	command := exec.Command(daemonPath)
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start %s: %w", daemonPath, err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("detach tbxd: %w", err)
	}
	return nil
}

func parseInterspersed(flags *flag.FlagSet, args []string) ([]string, error) {
	var flagArgs, positionals []string
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if argument == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			positionals = append(positionals, argument)
			continue
		}
		flagArgs = append(flagArgs, argument)
		nameValue := strings.TrimLeft(argument, "-")
		name, _, hasValue := strings.Cut(nameValue, "=")
		definition := flags.Lookup(name)
		if definition == nil || hasValue {
			continue
		}
		boolean, isBoolean := definition.Value.(interface{ IsBoolFlag() bool })
		if isBoolean && boolean.IsBoolFlag() {
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag needs an argument: %s", argument)
		}
		i++
		flagArgs = append(flagArgs, args[i])
	}
	if err := flags.Parse(flagArgs); err != nil {
		return nil, err
	}
	return positionals, nil
}
