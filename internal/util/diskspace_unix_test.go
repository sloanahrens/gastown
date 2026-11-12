//go:build !windows

package util

import (
	"syscall"
	"testing"
)

// TestGetDiskSpace_StatfsOnEveryOS pins the read to statfs: available space
// is Bavail blocks times the block size, on every OS.
func TestGetDiskSpace_StatfsOnEveryOS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		t.Fatalf("statfs %s: %v", dir, err)
	}
	info, err := GetDiskSpace(dir)
	if err != nil {
		t.Fatalf("GetDiskSpace(%q) failed: %v", dir, err)
	}
	if want := uint64(stat.Bavail) * uint64(stat.Bsize); info.AvailableBytes != want { //nolint:unconvert // Bavail is int64 on freebsd, uint64 on linux
		t.Errorf("AvailableBytes = %d, want Bavail*Bsize = %d", info.AvailableBytes, want)
	}
}
