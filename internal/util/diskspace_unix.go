//go:build !windows

package util

import (
	"fmt"
	"syscall"
)

// GetDiskSpace returns filesystem space information for the given path from
// statfs on every OS. Available space is Bavail blocks; APFS purgeable space
// is not counted on macOS, which gt no longer supports for spawns.
func GetDiskSpace(path string) (*DiskSpaceInfo, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, fmt.Errorf("statfs %s: %w", path, err)
	}
	return diskSpaceFromStatfs(stat), nil
}

// diskSpaceFromStatfs is the arithmetic of GetDiskSpace over one statfs result,
// apart from the call, so a test checks it on a value it builds instead of on a
// disk whose free space moves between two reads (gt-dgvuvb).
func diskSpaceFromStatfs(stat syscall.Statfs_t) *DiskSpaceInfo {
	bsize := uint64(stat.Bsize)
	total := stat.Blocks * bsize
	free := uint64(stat.Bavail) * bsize //nolint:unconvert // Bavail is int64 on freebsd, uint64 on linux
	used := total - (stat.Bfree * bsize)

	var usedPct float64
	if total > 0 {
		usedPct = float64(used) / float64(total) * 100
	}

	return &DiskSpaceInfo{
		AvailableBytes: free,
		TotalBytes:     total,
		UsedBytes:      used,
		UsedPercent:    usedPct,
	}
}
