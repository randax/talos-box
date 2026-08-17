//go:build !darwin && !linux

package imagecache

import "os"

// allocatedSize falls back to the apparent size on platforms without a stat
// block count: overstating a sparse file beats reporting nothing.
func allocatedSize(info os.FileInfo) int64 {
	return info.Size()
}
