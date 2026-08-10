//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
)

func requirePrivileges() error { return nil }

func resolveAllowedUID(explicit *uint32) (*uint32, error) {
	return linuxAllowedUID(explicit, uint32(os.Geteuid()), os.Getenv("SUDO_UID"))
}

func linuxAllowedUID(explicit *uint32, effectiveUID uint32, sudoUID string) (*uint32, error) {
	if explicit != nil {
		return explicit, nil
	}
	if effectiveUID != 0 {
		uid := effectiveUID
		return &uid, nil
	}
	if sudoUID == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseUint(sudoUID, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid SUDO_UID %q: %w", sudoUID, err)
	}
	uid := uint32(parsed)
	return &uid, nil
}

func warnMissingAllowedUID(allowedUID *uint32) {
	if allowedUID == nil {
		log.Print("warning: --allowed-uid is not configured and SUDO_UID is unavailable; only root can use tbx-helper")
	}
}
