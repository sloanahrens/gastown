package util

import (
	"errors"
	"strings"
	"testing"
)

// noAPFS stands in for diskutil where there is none: the query fails and
// the statfs numbers stand.
func noAPFS(string) (uint64, uint64, error) { return 0, 0, errors.New("no APFS container") }

func TestGetDiskSpace_CurrentDir(t *testing.T) {
	t.Parallel()
	info, err := getDiskSpace(".", noAPFS)
	if err != nil {
		t.Fatalf("getDiskSpace(\".\") failed: %v", err)
	}

	if info.TotalBytes == 0 {
		t.Error("TotalBytes should be > 0")
	}

	if info.AvailableBytes > info.TotalBytes {
		t.Errorf("AvailableBytes (%d) > TotalBytes (%d)", info.AvailableBytes, info.TotalBytes)
	}

	if info.UsedPercent < 0 || info.UsedPercent > 100 {
		t.Errorf("UsedPercent = %.1f, want 0-100", info.UsedPercent)
	}
}

func TestGetDiskSpace_InvalidPath(t *testing.T) {
	t.Parallel()
	_, err := GetDiskSpace("/nonexistent/path/that/should/not/exist")
	if err == nil {
		t.Error("expected error for invalid path, got nil")
	}
}

func TestDiskSpaceInfo_AvailableMB(t *testing.T) {
	t.Parallel()
	info := &DiskSpaceInfo{AvailableBytes: 1024 * 1024 * 512}
	if got := info.AvailableMB(); got != 512 {
		t.Errorf("AvailableMB() = %d, want 512", got)
	}
}

func TestDiskSpaceInfo_AvailableGB(t *testing.T) {
	t.Parallel()
	info := &DiskSpaceInfo{AvailableBytes: 1024 * 1024 * 1024 * 2}
	if got := info.AvailableGB(); got != 2.0 {
		t.Errorf("AvailableGB() = %f, want 2.0", got)
	}
}

func TestDiskSpaceInfo_AvailableHuman(t *testing.T) {
	t.Parallel()
	tests := []struct {
		bytes uint64
		want  string
	}{
		{500, "500 B"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{1024 * 1024 * 1024 * 2, "2.0 GB"},
	}

	for _, tt := range tests {
		info := &DiskSpaceInfo{AvailableBytes: tt.bytes}
		if got := info.AvailableHuman(); got != tt.want {
			t.Errorf("AvailableHuman() for %d bytes = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestFormatBytesHuman(t *testing.T) {
	t.Parallel()
	tests := []struct {
		bytes uint64
		want  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
		{1099511627776, "1.0 TB"},
	}

	for _, tt := range tests {
		got := FormatBytesHuman(tt.bytes)
		if got != tt.want {
			t.Errorf("FormatBytesHuman(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestDiskSpaceLevelThresholds(t *testing.T) {
	t.Parallel()
	const MB = uint64(1024 * 1024)
	for _, tc := range []struct {
		name      string
		availMB   uint64
		usedPct   float64
		want      DiskSpaceLevel
		msgPrefix string
	}{
		{"plenty", 50 * 1024, 50, DiskSpaceOK, ""},
		{"under the warning floor", DiskSpaceWarningMB - 1, 80, DiskSpaceWarning, "WARNING: only "},
		{"under the minimum", DiskSpaceMinimumMB - 1, 80, DiskSpaceCritical, "CRITICAL: only "},
		{"too full by percent", 50 * 1024, DiskSpaceCriticalPercent, DiskSpaceCritical, "CRITICAL: only "},
	} {
		info := &DiskSpaceInfo{AvailableBytes: tc.availMB * MB, TotalBytes: 1 << 40, UsedPercent: tc.usedPct}
		level, msg := diskSpaceLevel(info)
		if level != tc.want || !strings.HasPrefix(msg, tc.msgPrefix) || (tc.msgPrefix == "") != (msg == "") {
			t.Errorf("%s: diskSpaceLevel = %v %q, want %v %q...", tc.name, level, msg, tc.want, tc.msgPrefix)
		}
	}
}

func TestCheckDiskSpace_InvalidPath(t *testing.T) {
	t.Parallel()
	_, _, err := CheckDiskSpace("/nonexistent/path/that/should/not/exist")
	if err == nil {
		t.Error("expected error for invalid path, got nil")
	}
}

func TestDiskSpaceLevel_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level DiskSpaceLevel
		want  string
	}{
		{DiskSpaceOK, "ok"},
		{DiskSpaceWarning, "warning"},
		{DiskSpaceCritical, "critical"},
		{DiskSpaceLevel(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.level.String(); got != tt.want {
			t.Errorf("DiskSpaceLevel(%d).String() = %q, want %q", tt.level, got, tt.want)
		}
	}
}
