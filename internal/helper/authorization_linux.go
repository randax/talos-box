//go:build linux

package helper

import "os"

func helperSocketMode() os.FileMode { return 0o660 }

func isAuthorizedPeer(uid uint32, allowedUID *uint32) bool {
	// Linux authorization is enforced by the systemd-owned 0660 socket and its
	// tbx group. SO_PEERCRED still identifies every accepted connection. Keep
	// --allowed-uid as an optional stricter compatibility gate.
	return allowedUID == nil || isAuthorizedUID(uid, allowedUID)
}
