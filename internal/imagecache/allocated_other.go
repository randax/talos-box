//go:build !darwin && !linux

package imagecache

import "os"

// AllocatedSize falls back to the apparent size on platforms without a stat
// block count: overstating a sparse file beats reporting nothing.
func AllocatedSize(info os.FileInfo) int64 {
	return info.Size()
}
