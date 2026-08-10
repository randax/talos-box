//go:build !linux

package helper

import "os"

func helperSocketMode() os.FileMode { return 0o666 }

func isAuthorizedPeer(uid uint32, allowedUID *uint32) bool {
	return isAuthorizedUID(uid, allowedUID)
}
