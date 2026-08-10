//go:build linux

package helper

import "os"

func helperSocketMode() os.FileMode { return 0o666 }

func isAuthorizedPeer(uid uint32, allowedUID *uint32, allowAnyUID bool) bool {
	// The socket is connectable by every local user so access never depends on
	// deployment-specific group ownership. SO_PEERCRED is the authorization
	// boundary: root and the configured UID are the only accepted peers.
	return isAuthorizedUID(uid, allowedUID, allowAnyUID)
}
