package dashboard

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const machineTimeout = 3 * time.Second

// SampleMachine reads the load average and the busiest processes: two short
// children, run at most once per sample interval and only while a page is open.
func SampleMachine() (Machine, error) {
	ctx, cancel := context.WithTimeout(context.Background(), machineTimeout)
	defer cancel()
	m := Machine{At: time.Now()}
	out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return m, fmt.Errorf("vm.loadavg: %w", err)
	}
	if err := parseLoadavg(string(out), &m); err != nil {
		return m, err
	}
	// -r sorts by CPU on macOS; the first lines are the busiest.
	ps, err := exec.CommandContext(ctx, "/bin/ps", "-Ao", "pcpu=,comm=", "-r").Output()
	if err == nil {
		m.Top = parseTop(string(ps), 6)
	}
	return m, nil
}

// parseLoadavg reads sysctl's "{ 6.71 16.49 28.11 }".
func parseLoadavg(s string, m *Machine) error {
	f := strings.Fields(strings.Trim(strings.TrimSpace(s), "{}"))
	if len(f) < 3 {
		return fmt.Errorf("vm.loadavg: unexpected %q", s)
	}
	for i, dst := range []*float64{&m.Load1, &m.Load5, &m.Load15} {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return fmt.Errorf("vm.loadavg: %w", err)
		}
		*dst = v
	}
	return nil
}

// parseTop reads "pcpu comm" lines and keeps the first n that use any CPU,
// naming each process by the last element of its path and merging repeats
// (twenty go test children are one line: "go").
func parseTop(s string, n int) []Proc {
	var order []string
	sum := map[string]float64{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		cpu, err := strconv.ParseFloat(f[0], 64)
		if err != nil || cpu <= 0.5 {
			continue
		}
		name := f[1]
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if _, ok := sum[name]; !ok {
			order = append(order, name)
		}
		sum[name] += cpu
	}
	var out []Proc
	for _, name := range order {
		out = append(out, Proc{Name: name, CPU: sum[name]})
	}
	// Merging can reorder; keep the largest first.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CPU > out[j-1].CPU; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}
