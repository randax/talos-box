//go:build !linux

package main

import (
	"errors"
	"log"
	"os"

	"github.com/randax/talos-box/internal/helper"
)

func requirePrivileges() error {
	if os.Geteuid() != 0 {
		return errors.New("tbx-helper must run as root")
	}
	return nil
}

func resolveAllowedUID(explicit *uint32) (*uint32, error) { return explicit, nil }

func warnMissingAllowedUID(allowedUID *uint32) {
	if allowedUID == nil {
		log.Printf("warning: --allowed-uid is not configured; only root can use tbx-helper; %s from your account", helper.UnavailableAdvice())
	}
}
