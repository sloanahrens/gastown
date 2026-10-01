package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/util"
)

// diskSpaceCheck is a DiskSpaceCheck measuring info, or failing with err.
func diskSpaceCheck(info *util.DiskSpaceInfo, err error) *DiskSpaceCheck {
	c := NewDiskSpaceCheck()
	c.diskSpace = func(string) (*util.DiskSpaceInfo, error) { return info, err }
	return c
}

func TestDiskSpaceCheck_Run(t *testing.T) {
	t.Parallel()
	const gb = 1 << 30
	cases := []struct {
		name   string
		info   *util.DiskSpaceInfo
		status CheckStatus
		prefix string
	}{
		{"plenty", &util.DiskSpaceInfo{AvailableBytes: 100 * gb, TotalBytes: 500 * gb, UsedPercent: 80}, StatusOK, "100.0 GB free"},
		{"low", &util.DiskSpaceInfo{AvailableBytes: 800 << 20, TotalBytes: 500 * gb, UsedPercent: 90}, StatusWarning, "WARNING"},
		{"exhausted", &util.DiskSpaceInfo{AvailableBytes: gb / 4, TotalBytes: 500 * gb, UsedPercent: 99.9}, StatusError, "CRITICAL"},
	}
	for _, tc := range cases {
		result := diskSpaceCheck(tc.info, nil).Run(&CheckContext{TownRoot: "/town"})
		if result.Name != "disk-space" {
			t.Errorf("%s: Name = %q, want %q", tc.name, result.Name, "disk-space")
		}
		if result.Status != tc.status || !strings.HasPrefix(result.Message, tc.prefix) {
			t.Errorf("%s: got %v %q, want %v %q...", tc.name, result.Status, result.Message, tc.status, tc.prefix)
		}
	}
}

func TestDiskSpaceCheck_InvalidPath(t *testing.T) {
	t.Parallel()
	check := diskSpaceCheck(nil, errors.New("statfs /nonexistent: no such file or directory"))
	result := check.Run(&CheckContext{TownRoot: "/nonexistent"})

	if result.Status != StatusWarning {
		t.Errorf("Status = %v, want Warning for invalid path", result.Status)
	}

	if !strings.Contains(result.Message, "Could not check") {
		t.Errorf("Message = %q, want to contain 'Could not check'", result.Message)
	}
}

func TestDiskSpaceCheck_Properties(t *testing.T) {
	t.Parallel()
	check := NewDiskSpaceCheck()

	if check.Name() != "disk-space" {
		t.Errorf("Name() = %q, want %q", check.Name(), "disk-space")
	}

	if check.Description() == "" {
		t.Error("Description() should not be empty")
	}

	if check.CanFix() {
		t.Error("CanFix() should be false — disk space can't be auto-fixed")
	}

	if check.Category() != CategoryInfrastructure {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryInfrastructure)
	}
}
