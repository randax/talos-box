//go:build !darwin && !linux

package main

import (
	"errors"

	"github.com/randax/talos-box/internal/daemon"
)

func checkSystemDNS([]daemon.ClusterSummary, commandOutput) error {
	return errors.New("system DNS check is unsupported")
}
