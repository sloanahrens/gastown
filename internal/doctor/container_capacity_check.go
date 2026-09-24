package doctor

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ContainerCapacityCheck surfaces the CPU/memory bound of the Docker VM that
// container-backed test suites (Dolt containers, testcontainers patrol
// tests) run inside. Informational, but warns when the VM is below minContainerVMMemBytes.
// An operator comparing host-idle CPU against container contention needs to know
// the VM's actual size, since on Docker Desktop for Mac the VM is a fixed-size sandbox
// that host-idle readings do not reflect (see gt-bcsq — a single container was
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

// minContainerVMMemBytes is the smallest Docker VM that runs two full -p=8
// suites at once: one suite starts ~11 Dolt servers at 140-400 MiB each
// (claude-yfj). The VM reports ~3% less than its Docker Desktop setting, so
// 15 GiB here means "set at least 16 GiB".
const minContainerVMMemBytes int64 = 15 << 30

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
	msg := fmt.Sprintf("Docker VM: %d vCPU / %d MiB", ncpu, memMiB)
	details := []string{
		"This is the bound container-backed test suites (Dolt containers, testcontainers) share.",
		"Host-idle CPU does not reflect contention inside this VM — see 'gt slot' for the town-level gate that serializes container suites.",
	}
	if memBytes < minContainerVMMemBytes {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: msg + " — too small for concurrent container suites",
			Details: append([]string{
				"Two concurrent full suites exceed a smaller VM and Dolt-backed tests time out.",
				"See docs/plans/2026-09-24-test-suite-concurrency-design.md.",
			}, details...),
			FixHint: "Raise Docker Desktop memory to at least 16 GiB (Settings → Resources)",
		}
	}
	return &CheckResult{Name: c.Name(), Status: StatusOK, Message: msg, Details: details}
}
