//go:build integration && darwin

package util

import "testing"

// Against the real diskutil and statfs: the APFS numbers the unit tier
// stands in for.
func TestIntegrationGetDiskSpace_Darwin_APFS(t *testing.T) {
	info, err := GetDiskSpace(".")
	if err != nil {
		t.Fatalf("GetDiskSpace(\".\") failed: %v", err)
	}
	if info.TotalBytes == 0 {
		t.Error("TotalBytes should be > 0")
	}
	if info.AvailableBytes == 0 {
		t.Error("AvailableBytes should be > 0 on a non-full disk")
	}
	if info.AvailableBytes > info.TotalBytes {
		t.Errorf("AvailableBytes (%d) > TotalBytes (%d)", info.AvailableBytes, info.TotalBytes)
	}
	if info.UsedPercent < 0 || info.UsedPercent > 100 {
		t.Errorf("UsedPercent = %.1f, want 0-100", info.UsedPercent)
	}
}

func TestIntegrationApfsContainerSpace_CurrentMount(t *testing.T) {
	free, total, err := apfsContainerSpace("/System/Volumes/Data")
	if err != nil {
		t.Fatalf("apfsContainerSpace: %v", err)
	}
	if free == 0 {
		t.Error("expected non-zero free bytes from APFSContainerFree")
	}
	if total > 0 && free > total {
		t.Errorf("free (%d) > total (%d)", free, total)
	}
}
