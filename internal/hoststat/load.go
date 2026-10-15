package hoststat

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// loadAvg reads the host's three load averages, or three nils when the host
// reports none. It is one instant read of a counter the kernel already keeps,
// so unlike top it neither waits for a sample window nor fails when the host
// is too busy to answer in time — the case where the reading matters most
// (gt-auiji).
//
// It mirrors internal/land's hostLoad1 rather than calling it: both are the
// same two lines, and nothing here is downstream of land.
func loadAvg(ctx context.Context) (*float64, *float64, *float64) {
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		if l1, l5, l15 := parseProcLoadavg(string(data)); l1 != nil {
			return l1, l5, l15
		}
	}
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return nil, nil, nil
	}
	return parseSysctlLoadavg(string(out))
}

// parseProcLoadavg reads Linux's /proc/loadavg: "1.23 4.56 7.89 1/1234 56789",
// whose first three fields are the load averages and whose rest are the
// runnable count and the last pid.
func parseProcLoadavg(s string) (*float64, *float64, *float64) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return nil, nil, nil
	}
	return parseThree(f)
}

// parseSysctlLoadavg reads macOS's and the BSDs' `sysctl -n vm.loadavg`, which
// prints "{ 1.23 4.56 7.89 }".
func parseSysctlLoadavg(s string) (*float64, *float64, *float64) {
	return parseThree(strings.Fields(strings.Trim(strings.TrimSpace(s), "{ }")))
}

// parseThree reads three load averages off the front of a field list, or three
// nils when any of them is not a number.
func parseThree(f []string) (*float64, *float64, *float64) {
	if len(f) < 3 {
		return nil, nil, nil
	}
	var v [3]float64
	for i := range v {
		x, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return nil, nil, nil
		}
		v[i] = x
	}
	return &v[0], &v[1], &v[2]
}
