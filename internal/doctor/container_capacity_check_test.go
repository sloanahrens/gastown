package doctor

import (
	"errors"
	"strings"
	"testing"
)

func TestContainerCapacityCheck_SmallVMWarns(t *testing.T) {
	orig := dockerInfoCPUMem
	defer func() { dockerInfoCPUMem = orig }()
	dockerInfoCPUMem = func() (int, int64, error) { return 24, 8211824640, nil } // Docker Desktop at 8092 MiB

	result := NewContainerCapacityCheck().Run(&CheckContext{})

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
	orig := dockerInfoCPUMem
	defer func() { dockerInfoCPUMem = orig }()
	// A 16384 MiB setting reports ~1% under; it must not warn.
	dockerInfoCPUMem = func() (int, int64, error) { return 24, 16_600_000_000, nil }

	if got := NewContainerCapacityCheck().Run(&CheckContext{}).Status; got != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for a 16 GiB setting", got)
	}
}

func TestContainerCapacityCheck_DockerUnavailableIsSkipped(t *testing.T) {
	orig := dockerInfoCPUMem
	defer func() { dockerInfoCPUMem = orig }()

	dockerInfoCPUMem = func() (int, int64, error) {
		return 0, 0, errors.New("docker: command not found")
	}

	check := NewContainerCapacityCheck()
	result := check.Run(&CheckContext{})

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
