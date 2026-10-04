package land

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/util"
)

// hostLoad1 reads one raw 1-minute load average — unnormalized by core count,
// so a gate's time can be read against the load beside it; ok is false when
// the host reports none. It mirrors internal/daemon's loadAverage1 rather than
// calling it: daemon imports land.
func hostLoad1() (float64, bool) {
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		var load1 float64
		if _, err := fmt.Sscanf(string(data), "%f", &load1); err == nil {
			return load1, true
		}
	}
	cmd := exec.Command("/usr/sbin/sysctl", "-n", "vm.loadavg")
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	// macOS prints "{ 1.23 4.56 7.89 }".
	fields := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{ }"))
	if len(fields) == 0 {
		return 0, false
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return load1, true
}
