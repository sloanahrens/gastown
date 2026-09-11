package doctor

import (
	"errors"
	"strings"
	"testing"
)

func TestContainerCapacityCheck_ReportsVMSize(t *testing.T) {
	orig := dockerInfoCPUMem
	defer func() { dockerInfoCPUMem = orig }()

	dockerInfoCPUMem = func() (int, int64, error) {
		return 12, 8092 * 1024 * 1024, nil
	}

	check := NewContainerCapacityCheck()
	result := check.Run(&CheckContext{})

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK", result.Status)
	}
	if !strings.Contains(result.Message, "12 vCPU") || !strings.Contains(result.Message, "8092 MiB") {
		t.Fatalf("Message = %q, want it to report 12 vCPU / 8092 MiB", result.Message)
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
