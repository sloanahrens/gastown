package doctor

import (
	"errors"
	"strings"
	"testing"
)

func TestContainerCapacityCheck_SmallVMWarns(t *testing.T) {
	t.Parallel()
	info := func() (int, int64, error) { return 24, 8211824640, nil } // Docker Desktop at 8092 MiB

	result := capacityCheck(info).Run(&CheckContext{})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning for an ~8 GiB VM", result.Status)
	}
	if !strings.Contains(result.Message, "24 vCPU") || !strings.Contains(result.Message, "7831 MiB") {
		t.Errorf("Message = %q, want the measured size", result.Message)
	}
	if !strings.Contains(result.FixHint, "16 GiB") {
		t.Errorf("FixHint = %q, want the remedy (>= 16 GiB)", result.FixHint)
	}
	if joined := strings.Join(result.Details, "\n"); !strings.Contains(joined, "2026-09-24-test-suite-concurrency-design.md") {
		t.Errorf("Details = %q, want the design doc reference", joined)
	}
}

func TestContainerCapacityCheck_LargeVMIsOK(t *testing.T) {
	t.Parallel()
	// A 16384 MiB setting reports ~3% under; it must not warn.
	info := func() (int, int64, error) { return 24, 16_600_000_000, nil }

	if got := capacityCheck(info).Run(&CheckContext{}).Status; got != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for a 16 GiB setting", got)
	}
}

func TestContainerCapacityCheck_Boundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		memBytes int64
		want     CheckStatus
	}{
		{"at threshold is OK", 15 << 30, StatusOK},
		{"one byte under threshold warns", 15<<30 - 1, StatusWarning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info := func() (int, int64, error) { return 24, tc.memBytes, nil }
			if got := capacityCheck(info).Run(&CheckContext{}).Status; got != tc.want {
				t.Fatalf("Status = %v, want %v for memBytes=%d", got, tc.want, tc.memBytes)
			}
		})
	}
}

func TestContainerCapacityCheck_DockerUnavailableIsSkipped(t *testing.T) {
	t.Parallel()

	info := func() (int, int64, error) {
		return 0, 0, errors.New("docker: command not found")
	}

	result := capacityCheck(info).Run(&CheckContext{})

	if result.Status != StatusSkipped {
		t.Fatalf("Status = %v, want StatusSkipped (couldn't measure, not a clean pass)", result.Status)
	}
	if !strings.HasPrefix(result.Message, "unknown:") {
		t.Errorf("Message = %q, want it to start with %q", result.Message, "unknown:")
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "docker: command not found") {
		t.Errorf("Details = %v, want the underlying error", result.Details)
	}
}

// capacityCheck is a ContainerCapacityCheck measuring the VM with info.
func capacityCheck(info func() (int, int64, error)) *ContainerCapacityCheck {
	c := NewContainerCapacityCheck()
	c.dockerInfo = info
	return c
}
