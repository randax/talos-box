//go:build linux

package helper

import "os"

func helperSocketMode() os.FileMode { return 0o666 }

func isAuthorizedPeer(uid uint32, allowedUID *uint32, allowAnyUID bool) bool {
	// Two deployments, two boundaries. A self-hosted helper (--allowed-uid,
	// no activation) creates a world-connectable socket and SO_PEERCRED is the
	// boundary: root and the configured UID are the only accepted peers. A
	// socket-activated helper admits any peer (allowAnyUID): there the socket
	// unit's SocketMode=0660/SocketGroup=tbx is the boundary, pinned by
	// packaging/linuxassets/assets_test.go and nix/checks.nix.
	return isAuthorizedUID(uid, allowedUID, allowAnyUID)
}
