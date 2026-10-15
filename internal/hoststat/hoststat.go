// Package hoststat reads the host's busyness the way a load average cannot
// (gt-auiji).
//
// macOS counts runnable and uninterruptible-wait threads in its load
// average, not cores: on a 24-core machine running a Dolt server it reads 30
// with half the CPU idle, so it tracks thread counts rather than saturation.
// The readings here track harm instead. CPUIdle is the percent of all cores
// idle over the sample window and DoltCores is what the Dolt server itself
// is burning; the load averages ride along as the secondary number they are.
package hoststat

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// sampleTimeout bounds one Sample. top takes two samples a second apart, so
// this is several times the probe's own cost.
const sampleTimeout = 8 * time.Second

// topProcs is how many of the busiest processes a sample keeps.
const topProcs = 6

// Proc is one process's CPU in percent of one core.
type Proc struct {
	Name string
	CPU  float64
}

// Reading is one sample of the host. A number the sample could not take is
// nil, never zero: a CPU that went unread must not read as an idle one.
type Reading struct {
	// Load1, Load5 and Load15 are the load averages. macOS inflates them with
	// running threads, so they are secondary to CPUIdle.
	Load1, Load5, Load15 *float64
	// CPUIdle is the percent of all cores idle over the sample window.
	CPUIdle *float64
	// Cores is the host's logical CPU count.
	Cores int
	// DoltCores is the Dolt server's CPU, in cores.
	DoltCores *float64
	// Top is the busiest processes, largest first.
	Top []Proc
}

// Sample runs the host's two cheap probes and reports what they read: one
// top for the CPU idle reading and the load averages, one ps for the Dolt
// server's CPU and the busiest processes. It never fails; a probe that
// errors leaves the numbers it would have filled nil.
func Sample(ctx context.Context) Reading {
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	r := Reading{Cores: runtime.NumCPU()}
	if out, err := topOutput(ctx); err == nil {
		r.Load1, r.Load5, r.Load15 = parseLoadAvg(string(out))
		r.CPUIdle = parseCPUIdle(string(out))
	}
	if out, err := psOutput(ctx); err == nil {
		r.DoltCores = parseDoltCores(string(out))
		r.Top = parseTop(string(out), topProcs)
	}
	return r
}

// psOutput lists every process's CPU, busiest first.
func psOutput(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "/bin/ps", "-Ao", "pcpu=,comm=", "-r").Output()
}

// parseLoadAvg reads the three load averages out of top's last line that
// carries them: macOS writes "Load Avg: 1.2, 3.4, 5.6", Linux "top - 17:35
// up 3 days,  load average: 1.2, 3.4, 5.6". The last line wins because a
// two-sample top prints one per sample and the second is the reading of now.
func parseLoadAvg(s string) (*float64, *float64, *float64) {
	line := lastLineContaining(s, "load average")
	if line == "" {
		line = lastLineContaining(s, "load avg")
	}
	// The last colon: Linux's line opens with the clock ("top - 17:35:36 up
	// 3 days, ... load average: 1.2, 3.4, 5.6").
	i := strings.LastIndex(line, ":")
	if i < 0 {
		return nil, nil, nil
	}
	var out [3]*float64
	for n, f := range strings.Split(strings.TrimSpace(line[i+1:]), ",") {
		if n == len(out) {
			break
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
		if err != nil {
			return nil, nil, nil
		}
		out[n] = &v
	}
	if out[2] == nil {
		return nil, nil, nil
	}
	return out[0], out[1], out[2]
}

// parseCPUIdle reads the idle share out of top's last CPU line: macOS writes
// "CPU usage: 39.24% user, 60.75% sys, 0.0% idle", Linux "%Cpu(s):  6.2 us,
// 12.5 sy, 81.2 id". The last line wins for the same reason the load
// averages take the last one.
func parseCPUIdle(s string) *float64 {
	line := lastLineContaining(s, "cpu usage")
	if line == "" {
		line = lastLineContaining(s, "cpu(s)")
	}
	f := strings.Fields(line)
	for i, tok := range f {
		if i == 0 {
			continue
		}
		switch strings.Trim(tok, ",%") {
		case "idle", "id":
			v, err := strconv.ParseFloat(strings.Trim(f[i-1], ",%"), 64)
			if err != nil {
				return nil
			}
			return &v
		}
	}
	return nil
}

// parseDoltCores sums the CPU of the Dolt server's processes out of a "pcpu
// comm" listing, in cores. ps reports each process as a percent of one core,
// so 380 is 3.8 cores.
func parseDoltCores(s string) *float64 {
	var total float64
	var found bool
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || baseName(f[1]) != "dolt" {
			continue
		}
		cpu, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		total += cpu
		found = true
	}
	if !found {
		return nil
	}
	v := total / 100
	return &v
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
		name := baseName(f[1])
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

// baseName strips a path down to the program name.
func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// lastLineContaining returns the last line of s holding marker, ignoring
// case, or "" when none does.
func lastLineContaining(s, marker string) string {
	marker = strings.ToLower(marker)
	out := ""
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(strings.ToLower(line), marker) {
			out = line
		}
	}
	return out
}
