//go:build !linux

package main

import (
	"errors"
	"log"
	"os"
)

func requirePrivileges() error {
	if os.Geteuid() != 0 {
		return errors.New("tbx-helper must run as root")
	}
	return nil
}

func warnMissingAllowedUID(allowedUID *uint32) {
	if allowedUID == nil {
		log.Print("warning: --allowed-uid is not configured; only root can use tbx-helper; re-run `sudo tbx system install` from your account")
	}
}
