package doctor

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ContainerCapacityCheck surfaces the CPU/memory bound of the Docker VM that
// container-backed test suites (Dolt containers, testcontainers patrol
// tests) run inside. This is purely informational: an operator comparing
// host-idle CPU against container contention needs to know the VM's actual
// size, since on Docker Desktop for Mac the VM is a fixed-size sandbox that
// host-idle readings do not reflect (see gt-bcsq — a single container was
// measured at 99% CPU while host idle read 88%).
type ContainerCapacityCheck struct {
	BaseCheck
}

// NewContainerCapacityCheck creates a new container capacity check.
func NewContainerCapacityCheck() *ContainerCapacityCheck {
	return &ContainerCapacityCheck{
		BaseCheck: BaseCheck{
			CheckName:        "container-capacity",
			CheckDescription: "Show the Docker VM's CPU/memory bound for container-backed test suites",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// dockerInfoCPUMem lets tests substitute a fake docker CLI response.
var dockerInfoCPUMem = func() (ncpu int, memBytes int64, err error) {
	out, err := exec.Command("docker", "info", "--format", "{{.NCPU}} {{.MemTotal}}").Output() //nolint:gosec // G204: fixed args, no user input
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected docker info output: %q", string(out))
	}
	ncpu, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parsing NCPU: %w", err)
	}
	memBytes, err = strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parsing MemTotal: %w", err)
	}
	return ncpu, memBytes, nil
}

// Run reports the Docker VM's CPU/memory bound. It cannot distinguish
// "Docker isn't installed" from "Docker is installed but errored" — either
// way it could not measure the VM's capacity, so it reports StatusSkipped
// rather than claiming a clean result it never actually observed.
func (c *ContainerCapacityCheck) Run(_ *CheckContext) *CheckResult {
	ncpu, memBytes, err := dockerInfoCPUMem()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not determine Docker VM capacity",
			Details: []string{err.Error()},
		}
	}

	memMiB := memBytes / (1024 * 1024)
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("Docker VM: %d vCPU / %d MiB", ncpu, memMiB),
		Details: []string{
			"This is the bound container-backed test suites (Dolt containers, testcontainers) share.",
			"Host-idle CPU does not reflect contention inside this VM — see 'gt slot' for the town-level gate that serializes container suites.",
		},
	}
}
