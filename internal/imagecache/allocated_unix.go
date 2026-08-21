//go:build darwin || linux

package imagecache

import (
	"os"
	"syscall"
)

// AllocatedSize reports the bytes a file occupies on disk. Disk images are
// sparse, so their apparent size overstates the space they cost — often by an
// order of magnitude. The block count is the only honest number for capacity.
func AllocatedSize(info os.FileInfo) int64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.Size()
	}
	return int64(stat.Blocks) * 512
}
