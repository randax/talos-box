//go:build linux

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/helper"
)

func TestCloseListenersReportsRecoveryBeforeClearingTrackedListeners(t *testing.T) {
	first := &fakeDNSServer{}
	second := &fakeDNSServer{}
	service := &linuxDNSService{reconciler: &dnsReconciler{listeners: map[int]managedDNSListener{
		2: {cluster: "first", server: first},
		9: {cluster: "second", server: second},
	}}}

	err := service.closeListenersWithConnect(func() (*helper.Client, error) {
		return nil, errors.New("helper unavailable")
	})
	if err == nil {
		t.Fatal("close unexpectedly succeeded")
	}
	for _, want := range []string{
		"helper unavailable",
		"sudo resolvectl revert br-tbx2",
		"sudo resolvectl revert br-tbx9",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("close error %q does not contain %q", err, want)
		}
	}
	if first.closeCalls != 1 || second.closeCalls != 1 {
		t.Fatalf("close calls = %d, %d", first.closeCalls, second.closeCalls)
	}
	if len(service.reconciler.listeners) != 0 {
		t.Fatalf("listeners remain after close: %v", service.reconciler.listeners)
	}
}
