package main

import (
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/testpolicy"
)

// hostLoad1 is the host's 1-minute load average: /proc/loadavg on Linux,
// `sysctl -n vm.loadavg` elsewhere (macOS).
func hostLoad1() (float64, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		b, err = exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return 0, err
		}
	}
	return testpolicy.ParseLoad1(string(b))
}
