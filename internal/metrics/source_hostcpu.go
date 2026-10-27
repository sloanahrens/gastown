package metrics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The host_cpu group: what each consumer on the host burned, in cores, over a
// one-second window. A load spike gets a new theory every time it is looked at
// afterwards, so the record separates the consumers the town knows by name and
// ranks the rest:
//
//	polecat_vm_cores  the Virtualization.VirtualMachine of the colima gt-sbx
//	                  profile, the VM the polecats run in
//	docker_vm_cores   the Docker Desktop VM's virtualization process
//	dolt_cores        the Dolt server
//	scanner_cores     the security scanner, BDLDaemon
//	gt_cores          the town's gt processes, summed
//	other_1_name      the busiest other process name, then other_2 and
//	other_1_cores     other_3, each summing the processes of that name
//	window_s          the interval the two samples were taken over
//	unmeasured_pids   processes that exited between the samples: what they
//	                  burned after the first read is in no reading
//	regressed_pids    process ids whose two samples cannot be paired, because
//	                  the cumulative time fell or the program changed: the id
//	                  was reused, so their pair is in no reading
//
// Cores are the difference of each process's cumulative CPU time between two
// samples divided by the interval between them. ps's pcpu is never read: it is
// a decayed average that over-reads (it put Dolt at 2 cores when Dolt was
// using 0.7).
//
// A consumer with no process to measure is nil, never zero: a scanner that is
// stopped and one that could not be read must not read alike. A consumer whose
// process is there with no change is 0.0, which is a reading.
//
// Both samples are taken inside one Collect because the cores need each
// process's cumulative time in both. Per-consumer sums carried between records
// could not tell a process that exited (its whole total leaves the sum) from
// one that used nothing, and the sum falling would read as negative cores. A
// process-table read that fails leaves the whole group null with an errors
// entry, because a group of nils would read as a host with nothing running.
// Each read runs under hostCommandTimeout, so the group costs at most four of
// them and the one-second window, inside SourceTimeout.
type hostCPUSource struct{}

func (hostCPUSource) Group() string { return "host_cpu" }

func (hostCPUSource) Collect(ctx context.Context, env Env) (map[string]any, error) {
	if env.Wait == nil {
		return nil, errors.New("no wait seam, so the sampling window cannot be taken")
	}
	if err := hostCPUWindowFits(ctx, hostCPUWindow); err != nil {
		return nil, err
	}
	start := env.now()
	first, err := readHostCPUTable(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("first sample: %w", err)
	}
	env.Wait(hostCPUWindow)
	// The wait seam is a plain sleep, so the context can end inside it: the
	// second sample would then read a table for a record the snapshot has
	// already discarded.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("waiting out the sampling window: %w", err)
	}
	end := env.now()
	second, err := readHostCPUTable(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("second sample: %w", err)
	}
	window := end.Sub(start).Seconds()
	if window <= 0 {
		return nil, errors.New("the two samples share a timestamp, so there is no interval to divide by")
	}
	deltas, unmeasured, regressed := hostCPUDeltas(first, second, window)
	// A process belongs to the first consumer that matches it, so no two
	// columns count its cores twice.
	claimed := make([]bool, len(deltas))
	cols := make(map[string]any, len(hostCPUConsumers)+8)
	for _, consumer := range hostCPUConsumers {
		cols[consumer.column+"_cores"] = consumer.read(deltas, claimed)
	}
	ranked := hostCPURankOthers(deltas, claimed)
	for i := 1; i <= hostCPUOtherN; i++ {
		var name, cores any
		if i <= len(ranked) {
			name, cores = ranked[i-1].name, ranked[i-1].cores
		}
		cols[fmt.Sprintf("other_%d_name", i)] = name
		cols[fmt.Sprintf("other_%d_cores", i)] = cores
	}
	cols["window_s"] = window
	cols["unmeasured_pids"] = unmeasured
	cols["regressed_pids"] = regressed
	return cols, nil
}

// hostCPUWindow is the gap between the group's two samples: the one-second
// window top reads, long enough that a hundredth of a second of CPU time means
// something and short enough to cost the snapshot a second of its deadline.
const hostCPUWindow = time.Second

// hostCPUWindowFits refuses a window the group's deadline cannot hold, so a
// snapshot that is nearly out of time reads nothing. The wait seam sleeps for
// the whole window and cannot be interrupted: a wait that started with less
// time left would sleep past the deadline and spend a second process-table read
// on a record the snapshot has already discarded.
func hostCPUWindowFits(ctx context.Context, window time.Duration) error {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) >= window {
		return nil
	}
	return fmt.Errorf("the %s sampling window does not fit before the group's deadline", window)
}

// hostCPUOtherN is how many busiest other process names the group ranks: the
// other_N_name and other_N_cores columns run from 1 to this count.
const hostCPUOtherN = 3

// hostCPUNameArgv reads every process's cumulative CPU time and its program
// path. time is user plus system CPU since the process started, the counter
// the cores are the rate of. comm is the last field because a path may hold
// spaces ("Google Chrome Helper"), and everything after the CPU time is the
// path.
var hostCPUNameArgv = []string{"ps", "-axo", "pid=,time=,comm="}

// hostCPUArgsArgv reads every process's command line. The two VMs are both
// Virtualization.VirtualMachine processes whose paths tell them apart by
// nothing, so the profile and the Docker path in their arguments are the
// reading that separates them (gt-b9j3o.12).
var hostCPUArgsArgv = []string{"ps", "-axo", "pid=,args="}

// hostCPUConsumer is one named consumer: the column stem its cores are
// written under and the argument that claims its processes.
type hostCPUConsumer struct {
	column string
	match  func(hostCPUDelta) bool
}

// hostCPUConsumers are the consumers the bead names, in the order the record
// reads them. On macOS both VMs are Virtualization.VirtualMachine processes,
// so the colima profile and the Docker path in the command line separate them;
// the others are matched by program name.
var hostCPUConsumers = []hostCPUConsumer{
	{
		column: "polecat_vm",
		match: func(p hostCPUDelta) bool {
			return strings.Contains(p.command, "Virtualization.VirtualMachine") &&
				strings.Contains(p.command, "gt-sbx")
		},
	},
	{
		column: "docker_vm",
		match: func(p hostCPUDelta) bool {
			return strings.Contains(p.command, "com.docker.virtualization") ||
				(strings.Contains(p.command, "Virtualization.VirtualMachine") &&
					strings.Contains(p.command, "com.docker."))
		},
	},
	{column: "dolt", match: func(p hostCPUDelta) bool { return p.name == "dolt" }},
	{column: "scanner", match: func(p hostCPUDelta) bool { return p.name == "BDLDaemon" }},
	{column: "gt", match: func(p hostCPUDelta) bool { return p.name == "gt" }},
}

// read sums the consumer's processes over the window and marks them claimed.
// nil means none of them was measured: they were gone by the second sample, or
// none was running.
func (c hostCPUConsumer) read(deltas []hostCPUDelta, claimed []bool) any {
	sum, measured := 0.0, false
	for i, d := range deltas {
		if claimed[i] || !c.match(d) {
			continue
		}
		claimed[i] = true
		sum += d.cores
		measured = true
	}
	if !measured {
		return nil
	}
	return sum
}

// hostCPUProc is one process in one sample.
type hostCPUProc struct {
	// path is the program the process runs, which tells a reused id from the
	// process that held it in the other sample.
	path    string
	name    string
	command string
	// seconds is the process's cumulative CPU time since it started.
	seconds float64
}

// hostCPUDelta is one process's CPU over the window, in cores.
type hostCPUDelta struct {
	pid     int
	name    string
	command string
	cores   float64
}

// hostCPUDeltas pairs the two samples by process id and turns each pair's
// cumulative-time difference into cores. A process only in the second sample
// started inside the window, so its whole cumulative time was burned in it; a
// process only in the first exited, and what it burned after that read is
// unknown, so it is counted in unmeasured and contributes nothing. A
// difference below zero, or a program that differs between the samples, means
// the id was reused: the two totals belong to two processes, so neither is a
// reading of this window's other one and the pair is counted in regressed. The
// deltas come back sorted by id, so a consumer's sum does not depend on map
// order.
func hostCPUDeltas(first, second map[int]hostCPUProc, window float64) (deltas []hostCPUDelta, unmeasured, regressed int) {
	for pid, after := range second {
		burnt := after.seconds
		if before, ok := first[pid]; ok {
			if before.path != after.path {
				// The id names another program in the second sample, so the new
				// process's total is not the old one's plus a window's burn.
				regressed++
				continue
			}
			burnt = after.seconds - before.seconds
			if burnt < 0 {
				regressed++
				continue
			}
		}
		deltas = append(deltas, hostCPUDelta{
			pid:     pid,
			name:    after.name,
			command: after.command,
			cores:   burnt / window,
		})
	}
	for pid := range first {
		if _, ok := second[pid]; !ok {
			unmeasured++
		}
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i].pid < deltas[j].pid })
	return deltas, unmeasured, regressed
}

// hostCPURanked is one process name among the ones no named consumer claimed.
type hostCPURanked struct {
	name  string
	cores float64
}

// hostCPURankOthers sums the processes no consumer claimed by program name and
// returns the three busiest. A name at zero cores consumed nothing measurable,
// so an idle host ranks none rather than naming whatever was merely present;
// equal cores rank by name, so the same host ranks the same way every run.
func hostCPURankOthers(deltas []hostCPUDelta, claimed []bool) []hostCPURanked {
	sums := map[string]float64{}
	for i, d := range deltas {
		if claimed[i] || d.cores <= 0 {
			continue
		}
		sums[d.name] += d.cores
	}
	ranked := make([]hostCPURanked, 0, len(sums))
	for name, cores := range sums {
		ranked = append(ranked, hostCPURanked{name: name, cores: cores})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].cores != ranked[j].cores {
			return ranked[i].cores > ranked[j].cores
		}
		return ranked[i].name < ranked[j].name
	})
	if len(ranked) > hostCPUOtherN {
		ranked = ranked[:hostCPUOtherN]
	}
	return ranked
}

// readHostCPUTable takes one sample: the names and cumulative CPU times, then
// the command lines the two VMs are told apart by. The reads cover the same
// processes and are joined by pid; a process the command-line read missed keeps
// its path as its command, which costs only the VM match that needs arguments.
func readHostCPUTable(ctx context.Context, env Env) (map[int]hostCPUProc, error) {
	out, err := runHostCPUPS(ctx, env, hostCPUNameArgv)
	if err != nil {
		return nil, fmt.Errorf("reading the process names: %w", err)
	}
	procs, err := parseHostCPUNameProcs(string(out))
	if err != nil {
		return nil, err
	}
	out, err = runHostCPUPS(ctx, env, hostCPUArgsArgv)
	if err != nil {
		return nil, fmt.Errorf("reading the command lines: %w", err)
	}
	lines, err := parseHostCPUArgs(string(out))
	if err != nil {
		return nil, err
	}
	for pid, proc := range procs {
		if line, ok := lines[pid]; ok {
			proc.command = line
			procs[pid] = proc
		}
	}
	return procs, nil
}

// runHostCPUPS runs one process-table read under hostCommandTimeout.
func runHostCPUPS(ctx context.Context, env Env, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, hostCommandTimeout)
	defer cancel()
	return env.Run(ctx, argv)
}

// parseHostCPUNameProcs reads pid=,time=,comm= output into one entry per
// process. A line that does not start with a process id and a CPU time is an
// error rather than a skipped line: a table read in another shape would drop
// consumers silently and understate the host.
func parseHostCPUNameProcs(out string) (map[int]hostCPUProc, error) {
	procs := make(map[int]hostCPUProc)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidField, rest := cutHostCPUField(line)
		pid, err := strconv.Atoi(pidField)
		if err != nil {
			return nil, fmt.Errorf("the process names name no process id: %q: %w", pidField, err)
		}
		timeField, path := cutHostCPUField(rest)
		seconds, err := parseHostCPUTime(timeField)
		if err != nil {
			return nil, fmt.Errorf("process %d: %w", pid, err)
		}
		if path == "" {
			return nil, fmt.Errorf("process %d names no program", pid)
		}
		procs[pid] = hostCPUProc{path: path, name: hostCPUName(path), command: path, seconds: seconds}
	}
	if len(procs) == 0 {
		return nil, errors.New("the process table named no process")
	}
	return procs, nil
}

// parseHostCPUArgs reads pid=,args= output into each process's command line.
// The line runs to the end of the row, arguments and all.
func parseHostCPUArgs(out string) (map[int]string, error) {
	lines := make(map[int]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidField, command := cutHostCPUField(line)
		pid, err := strconv.Atoi(pidField)
		if err != nil {
			return nil, fmt.Errorf("the command lines name no process id: %q: %w", pidField, err)
		}
		if command == "" {
			continue
		}
		lines[pid] = command
	}
	if len(lines) == 0 {
		return nil, errors.New("the command-line table named no process")
	}
	return lines, nil
}

// cutHostCPUField splits the first whitespace-separated field off s and trims
// the rest.
func cutHostCPUField(s string) (string, string) {
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeftFunc(s[i:], unicode.IsSpace)
}

// parseHostCPUTime reads ps's TIME column: [[dd-]hh:]mm:ss, printed with
// hundredths on macOS ("0:00.05") and in whole seconds on Linux ("00:00"). A
// field that is not this shape is an error, so a changed column reads as
// unreadable rather than as zero CPU.
func parseHostCPUTime(field string) (float64, error) {
	rest := field
	days := 0.0
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		d, err := strconv.Atoi(rest[:i])
		if err != nil {
			return 0, fmt.Errorf("CPU time %q: %w", field, err)
		}
		days = float64(d)
		rest = rest[i+1:]
	}
	parts := strings.Split(rest, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("CPU time %q is not [[dd-]hh:]mm:ss", field)
	}
	clock := 0.0
	for _, part := range parts {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("CPU time %q: %w", field, err)
		}
		clock = clock*60 + v
	}
	return days*86400 + clock, nil
}

// hostCPUName is the program name a process is ranked and consumer-matched by:
// the last element of its executable path, which may hold spaces.
func hostCPUName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}
