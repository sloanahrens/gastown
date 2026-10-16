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
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// sampleTimeout bounds one Sample. top takes two samples a second apart, so
// this is several times the probe's own cost.
const sampleTimeout = 8 * time.Second

// loadTimeout bounds the instant load read. It is one sysctl, so anything
// near this means the read is not coming back.
const loadTimeout = 2 * time.Second

// topProcs is how many of the busiest processes a sample keeps.
const topProcs = 6

// CPUIdleScarce and CPUIdleCritical are the CPU idle shares below which a
// host is short of CPU and nearly out of it. The idle share states that, not
// the load average: macOS counts running threads into the load average, so a
// 24-core host with half its CPU idle reads load 30 (gt-auiji). They live
// here because the two readers of the idle share have to turn at the same
// number — a slow exec probe on a host below CPUIdleScarce is timing a run
// queue rather than the macOS scan (gt-v50sn), which is the point the
// dashboard's host tile turns amber at.
const (
	CPUIdleScarce   = 20.0
	CPUIdleCritical = 5.0
)

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

// probes are the three commands one sample runs: the kernel's load counters,
// top for the idle share, ps for the process table. They are a struct rather
// than three arguments so a test can answer the one probe it is about and
// leave the others reading a host.
type probes struct {
	load func(context.Context) (*float64, *float64, *float64)
	top  func(context.Context) ([]byte, error)
	ps   func(context.Context) ([]byte, error)
}

// hostProbes are the real ones.
var hostProbes = probes{load: loadAvg, top: topOutput, ps: psOutput}

// Sample runs the host's three cheap probes and reports what they read. It
// never fails; a probe that errors leaves the numbers it would have filled
// nil, each probe independently, so one that is slow or refused costs only
// its own reading (gt-auiji).
func Sample(ctx context.Context) Reading { return sample(ctx, hostProbes) }

// sample is Sample over a set of probes.
func sample(ctx context.Context, p probes) Reading {
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	r := Reading{Cores: runtime.NumCPU()}
	// The load averages come from the kernel's own counters, never from top:
	// they are one instant read rather than a sample window, so a top that
	// turns slow or is killed by the timeout cannot take them down with it.
	// The tile would then age out and lose the load average along with the
	// idle reading (gt-auiji).
	r.Load1, r.Load5, r.Load15 = p.load(ctx)
	if out, err := p.top(ctx); err == nil {
		r.CPUIdle = parseCPUIdle(string(out))
	}
	if out, err := p.ps(ctx); err == nil {
		r.DoltCores = parseDoltCores(string(out))
		r.Top = parseTop(string(out), topProcs)
	}
	return r
}

// SampleCPUIdle reads the CPU idle share alone: one two-sample top and its
// second sample, none of the load or process-table probes a full Sample also
// runs. It is the reading for a caller that gates on idle CPU and needs
// nothing else, such as the daemon's spawn pressure gate (gt-kc8v1). A non-nil
// error means top could not be read, which a caller must not take for an idle
// host; it says whether top failed or timed out, or ran and printed no idle
// share.
func SampleCPUIdle(ctx context.Context) (float64, error) {
	return sampleCPUIdle(ctx, hostProbes.top)
}

// sampleCPUIdle is SampleCPUIdle over a top probe.
func sampleCPUIdle(ctx context.Context, top func(context.Context) ([]byte, error)) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	out, err := top(ctx)
	if err != nil {
		// A killed top reports "signal: killed", not the context's error, so
		// the context says which of the two ended it.
		switch ctxErr := ctx.Err(); {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			return 0, fmt.Errorf("top did not finish within %s: %w", sampleTimeout, ctxErr)
		case ctxErr != nil:
			return 0, fmt.Errorf("top was canceled before it finished: %w", ctxErr)
		}
		return 0, fmt.Errorf("top failed: %w", err)
	}
	idle := parseCPUIdle(string(out))
	if idle == nil {
		return 0, errors.New("top ran but printed no CPU idle share")
	}
	return *idle, nil
}

// psOutput lists every process's CPU and command line. The portable -axo form
// is the one internal/land uses: the `-r` a BSD ps sorts by is not a flag but
// a selection on Linux ("only running processes" in a GNU ps), which would
// leave a sleeping Dolt server out of the sample (gt-auiji).
func psOutput(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "ps", "-axo", "pcpu=,command=").Output()
}

// parseCPUIdle reads the idle share out of top's last CPU line: macOS writes
// "CPU usage: 39.24% user, 60.75% sys, 0.0% idle", Linux "%Cpu(s):  6.2 us,
// 12.5 sy, 81.2 id". The last line wins because a two-sample top prints one
// per sample, and the first is the host's average since boot.
func parseCPUIdle(s string) *float64 {
	line := lastLineContaining(s, "cpu usage")
	if line == "" {
		line = lastLineContaining(s, "cpu(s)")
	}
	return parseIdleFields(strings.Fields(line))
}

// parseIdleFields reads the idle share off one CPU line's fields. Both
// procps-ng field layouts are here: the one whose value is its own token
// ("6.2 us, 12.5 sy, 81.2 id"), and the older printf-style one that carries
// the unit in the name ("6.2%us, 12.5%sy, 81.2%id") (gt-6rieh).
func parseIdleFields(f []string) *float64 {
	for i, tok := range f {
		if i == 0 {
			continue
		}
		tok = strings.TrimSuffix(tok, ",")
		switch tok {
		case "idle", "id":
			return idleValue(f[i-1])
		}
		for _, unit := range []string{"%idle", "%id"} {
			if !strings.HasSuffix(tok, unit) {
				continue
			}
			if v, err := strconv.ParseFloat(strings.TrimSuffix(tok, unit), 64); err == nil {
				return &v
			}
		}
	}
	return nil
}

// idleValue reads the number before an "idle"/"id" field, which top may hang
// its own punctuation off.
func idleValue(tok string) *float64 {
	v, err := strconv.ParseFloat(strings.Trim(tok, ",%"), 64)
	if err != nil {
		return nil
	}
	return &v
}

// parseDoltCores sums the CPU of the Dolt server's processes out of a "pcpu
// command" listing, in cores. ps reports each process as a percent of one
// core, so 380 is 3.8 cores.
//
// Only the server counts. `dolt` is also the client: the one-shot its backup
// and `dolt sql` run pay for their own work and are gone by the next sample,
// so counting every process named dolt would read a backup's CPU as the
// server's (gt-auiji).
func parseDoltCores(s string) *float64 {
	var total float64
	var found bool
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || baseName(f[1]) != "dolt" || f[2] != "sql-server" {
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

// parseTop reads "pcpu command" lines and keeps the first n that use any CPU,
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
