//go:build !windows

package util

import (
	"syscall"
	"testing"
)

// TestDiskSpaceFromStatfs pins the arithmetic on a statfs result the test builds:
// available space is Bavail blocks times the block size, total is Blocks times
// it, used is total less Bfree blocks, on every OS. A value it builds does not
// move, unlike the free space of a disk another process is writing to
// (gt-dgvuvb).
func TestDiskSpaceFromStatfs(t *testing.T) {
	t.Parallel()
	var stat syscall.Statfs_t
	stat.Bsize = 4096
	stat.Blocks = 1000
	stat.Bfree = 400
	stat.Bavail = 300

	info := diskSpaceFromStatfs(stat)

	if want := uint64(300 * 4096); info.AvailableBytes != want {
		t.Errorf("AvailableBytes = %d, want Bavail*Bsize = %d", info.AvailableBytes, want)
	}
	if want := uint64(1000 * 4096); info.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want Blocks*Bsize = %d", info.TotalBytes, want)
	}
	if want := uint64(600 * 4096); info.UsedBytes != want {
		t.Errorf("UsedBytes = %d, want (Blocks-Bfree)*Bsize = %d", info.UsedBytes, want)
	}
	if info.UsedPercent != 60 {
		t.Errorf("UsedPercent = %v, want 60", info.UsedPercent)
	}
}

// TestDiskSpaceFromStatfsEmptyFilesystem: a filesystem that reports no blocks has
// no percent to compute and does not divide by zero.
func TestDiskSpaceFromStatfsEmptyFilesystem(t *testing.T) {
	t.Parallel()
	var stat syscall.Statfs_t
	stat.Bsize = 4096

	info := diskSpaceFromStatfs(stat)

	if info.TotalBytes != 0 || info.UsedPercent != 0 {
		t.Errorf("empty filesystem = %+v, want total 0 and percent 0", info)
	}
}

// TestGetDiskSpace_StatfsOnEveryOS reads a real directory and asserts only what
// holds however the disk moves while the test runs: the read succeeds, the disk
// has a size, the free space fits in it and is whole blocks. It compares against
// no second read, because free space changes between two reads on a busy disk
// (gt-dgvuvb).
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
	if info.TotalBytes == 0 {
		t.Fatal("TotalBytes = 0 for a real directory")
	}
	if info.AvailableBytes > info.TotalBytes {
		t.Errorf("AvailableBytes %d is more than TotalBytes %d", info.AvailableBytes, info.TotalBytes)
	}
	if bsize := uint64(stat.Bsize); bsize == 0 || info.AvailableBytes%bsize != 0 {
		t.Errorf("AvailableBytes %d is not a whole number of %d-byte blocks", info.AvailableBytes, stat.Bsize)
	}
}
