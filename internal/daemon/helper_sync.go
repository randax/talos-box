package daemon

import (
	"fmt"
	"log"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/helper"
)

// helperSyncClient is the slice of the helper client this daemon needs to hand
// the helper its copy of the cluster reservations.
type helperSyncClient interface {
	Sync([]cluster.Cluster) error
	Close() error
}

var (
	connectSyncHelper   = func() (helperSyncClient, error) { return helper.Connect() }
	listClustersForSync = cluster.List
)

// SyncHelperState pushes every cluster's reservations to the helper. tbxd owns
// cluster state — the packaged Linux helper runs as a system user that cannot
// read the caller's home — so without this the helper serves no DHCP and
// rebuilds no bridges.
func SyncHelperState() error {
	clusters, err := listClustersForSync()
	if err != nil {
		return fmt.Errorf("sync helper state: %w", err)
	}
	client, err := connectSyncHelper()
	if err != nil {
		return fmt.Errorf("sync helper state: %w", helperInstallError(err))
	}
	defer func() { _ = client.Close() }()
	if err := client.Sync(clusters); err != nil {
		return fmt.Errorf("sync helper state: %w", err)
	}
	return nil
}

// logHelperSyncFailure records a sync that could not be delivered on a teardown
// path. Nothing is booting there, so a stale helper copy is corrected by the
// next sync rather than failing the verb the operator just completed.
func logHelperSyncFailure(context string) {
	if err := SyncHelperState(); err != nil {
		log.Printf("%s: %v", context, err)
	}
}
