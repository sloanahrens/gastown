package metrics

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

// hostCommandTimeout bounds each host probe separately, so a wedged top,
// sysctl, bead read, lsof or docker costs its own column and leaves the others
// filled.
const hostCommandTimeout = 5 * time.Second

// hostSource is the host group: the machine the snapshot describes, read with
// the macOS commands the operator uses by hand, plus one timed bead read for
// what bd costs it. In container mode the CPU share comes from the pod's cgroup
// and the Docker Desktop disk from the host agent's probe file, because sysctl,
// top, lsof and docker inside a container read the container (gt-jd1fcj). A
// probe that cannot give a reading leaves its column nil rather than inventing
// a number, and the failure is counted in
// host_probe_failures so a column that is nil because its command failed, hung,
// printed nothing or printed something the parser could not read is a number in
// the record and not a silent absence. The other columns are untouched: one
// broken command costs one reading, never the group.
type hostSource struct {
	// timeout bounds each probe; zero means hostCommandTimeout. Tests set it
	// short to exercise the deadline itself.
	timeout time.Duration
	// doltPort is the port the Dolt server listens on. lsof counts the
	// connections to it, which answers without a database round trip, so a
	// server too busy to answer still reports its connection count.
	doltPort string
	// dockerContainer is a container whose filesystem is the Docker VM's disk,
	// so df inside it reads the usage Docker Desktop fills (gt-mexpf). The
	// default is hostprobe.DockerDiskContainer, the container the host agent
	// probes for the same disk: the runner container the column used to name
	// was retired with the Docker Forgejo stack (gt-gg9hv.35).
	dockerContainer string
}

// newHostSource is the host group as the registry runs it: the default Dolt
// port and the town's Dolt container, which is on the Docker Desktop disk the
// column measures.
func newHostSource() hostSource {
	return hostSource{
		doltPort:        strconv.Itoa(doltserver.DefaultPort),
		dockerContainer: hostprobe.DockerDiskContainer,
	}
}

func (hostSource) Group() string { return "host" }

// Collect fills the host group: in container mode from the pod's cgroup and the
// host agent's file, from the Mac's own commands otherwise. A probe that cannot
// give a reading is a nil column counted in host_probe_failures; only an Env
// with no bead store, which is a wiring error rather than a host that could not
// answer, is an error.
func (s hostSource) Collect(ctx context.Context, env Env) (map[string]any, error) {
	if env.Beads == nil {
		return nil, fmt.Errorf("host: the environment has no bead store")
	}
	if env.HostProbes != nil {
		return s.collectInContainer(ctx, env)
	}
	probes := s.probes()
	cols := make(map[string]any, len(probes)+1)
	failures := 0
	for _, probe := range probes {
		value, ok := probe.read(ctx, env, s.probeTimeout())
		cols[probe.column] = value
		if !ok {
			failures++
		}
	}
	cols["host_probe_failures"] = failures
	return cols, nil
}

// probeTimeout is the deadline each probe gets.
func (s hostSource) probeTimeout() time.Duration {
	if s.timeout > 0 {
		return s.timeout
	}
	return hostCommandTimeout
}

// collectInContainer fills the group for a process inside the town's container.
// None of the macOS commands runs: their answer would describe the container.
// cpu_idle_pct is the pod's own CPU, sampled from its cgroup, because the host
// agent's idle share is the Mac's and the container cannot see it. docker_disk_pct
// is the one field still read from the host agent's probe file, since only the
// Mac can ask Docker Desktop for its VM disk. The bead read stays, because it
// measures this process's own path to the store, the one its other reads take.
//
// load1 and dolt_conns are nil without being failures: nothing in the container
// or the file gives them, and the container's own would name the container, so
// no probe for them runs and host_probe_failures counts only the facts read
// here. A fact that could not be read is nil and counted, never the zero it
// would decode to.
func (s hostSource) collectInContainer(ctx context.Context, env Env) (map[string]any, error) {
	p, err := env.HostProbes(env.now())
	cols := map[string]any{"load1": nil, "dolt_conns": nil}
	failures := 0
	cpuCtx, cancel := context.WithTimeout(ctx, s.probeTimeout())
	cpu, cpuErr := podCPUIdlePct(cpuCtx, env)
	cancel()
	if cpuErr != nil {
		cols["cpu_idle_pct"] = nil
		failures++
	} else {
		cols["cpu_idle_pct"] = cpu
	}
	if err != nil || p.DockerDisk.Err != "" {
		cols["docker_disk_pct"] = nil
		failures++
	} else {
		cols["docker_disk_pct"] = p.DockerDisk.Value.UsedPct
	}
	value, ok := hostLatencyProbe.read(ctx, env, s.probeTimeout())
	cols["bd_latency_ms"] = value
	if !ok {
		failures++
	}
	cols["host_probe_failures"] = failures
	return cols, nil
}

// hostProbesFromFile reads the probe file the host agent publishes under town,
// the host group's source for the Docker Desktop disk in container mode. A file
// the reader refuses is an error, so the group reports its disk as unknown
// rather than as zero.
func hostProbesFromFile(town string) func(now time.Time) (hostprobe.Probes, error) {
	path := hostprobe.Path(town)
	return func(now time.Time) (hostprobe.Probes, error) {
		return hostprobe.Read(path, now, hostprobe.ProbeFileMaxAge)
	}
}

// The pod's cgroup v2 files. The pod's CPU is accounted to its own cgroup, so
// the counter and the quota describe the pod the daemon runs in, which is the
// CPU the container can see, not the Mac under it.
const (
	podCPUStatPath = "/sys/fs/cgroup/cpu.stat"
	podCPUMaxPath  = "/sys/fs/cgroup/cpu.max"
)

// podCPUIdlePct reads the idle share of the pod's CPU over one window. The
// cumulative usage counter is read on either side of the window, and the burn
// is the share of the pod's cores (cpu.max, or every core the node has when the
// pod has no quota) that the window's wall time could have used. The window is
// hostCPUWindow, waited through env.Wait, so the group takes that second too.
//
// A cgroup file that cannot be read, a counter that fell between the samples and
// a window that did not move are errors: each would make the idle share a number
// nothing measured.
func podCPUIdlePct(ctx context.Context, env Env) (float64, error) {
	if env.Wait == nil || env.ReadFile == nil {
		return 0, errors.New("no cgroup read or wait seam, so the pod's CPU cannot be sampled")
	}
	cores, err := podCPUCores(env)
	if err != nil {
		return 0, err
	}
	start := env.now()
	first, err := podCPUUsage(env)
	if err != nil {
		return 0, err
	}
	env.Wait(hostCPUWindow)
	// The wait seam is a plain sleep, so the context can end inside it: the
	// second sample would then read a counter for a record already discarded.
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("waiting out the sampling window: %w", err)
	}
	end := env.now()
	second, err := podCPUUsage(env)
	if err != nil {
		return 0, err
	}
	window := end.Sub(start).Seconds()
	if window <= 0 {
		return 0, errors.New("the two samples share a timestamp, so there is no window to divide by")
	}
	if second < first {
		return 0, errors.New("the pod's cpu.stat usage fell between samples, so it is not one cgroup's counter")
	}
	busy := 100 * (float64(second-first) / 1e6) / (window * cores)
	// The counter and the wall clock round against each other, so a sample can
	// read a little past every core busy. That is fully busy, not negative idle.
	if busy > 100 {
		busy = 100
	}
	return 100 - busy, nil
}

// podCPUCores is how many cores the pod may use: the quota over the period in
// cpu.max, which reads "max <period>" when the pod has no quota. A pod with no
// quota may use every core the node has.
func podCPUCores(env Env) (float64, error) {
	raw, err := env.ReadFile(podCPUMaxPath)
	if err != nil {
		return 0, fmt.Errorf("reading the pod's cpu.max: %w", err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return 0, fmt.Errorf("cpu.max names %d fields, want a quota and a period", len(fields))
	}
	if fields[0] == "max" {
		return float64(runtime.NumCPU()), nil
	}
	quota, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("cpu.max quota %q: %w", fields[0], err)
	}
	period, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, fmt.Errorf("cpu.max period %q: %w", fields[1], err)
	}
	if quota <= 0 || period <= 0 {
		return 0, errors.New("cpu.max names no cores")
	}
	return quota / period, nil
}

// podCPUUsage reads the pod's cumulative CPU time, in microseconds, from the
// usage_usec line of cpu.stat.
func podCPUUsage(env Env) (int64, error) {
	raw, err := env.ReadFile(podCPUStatPath)
	if err != nil {
		return 0, fmt.Errorf("reading the pod's cpu.stat: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "usage_usec" {
			continue
		}
		usage, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cpu.stat usage_usec %q: %w", fields[1], err)
		}
		return usage, nil
	}
	return 0, errors.New("cpu.stat names no usage_usec")
}

// hostProbe is one host column: the command that reads it and the parser that
// turns its stdout into the value. store marks the probe whose read is the
// issue store rather than a command: its value is the time the read took.
type hostProbe struct {
	column string
	argv   []string
	store  bool
	parse  func(string) (any, error)
}

// hostIdleProbe reads the host's idle share. The burst sampler runs it alone
// (BurstRow), so the snapshot's host group and the burst row read idle from one
// command and one parser.
var hostIdleProbe = hostProbe{
	column: "cpu_idle_pct",
	argv:   []string{"/usr/bin/top", "-l", "2", "-n", "0", "-s", "1"},
	parse:  parseCPUIdlePct,
}

// hostLatencyProbe times one list of one issue in the town's store. Both paths
// run it: a container's bead reads take it over the network to the store, so
// the number is the container's own cost, not a reading of the Mac.
var hostLatencyProbe = hostProbe{
	column: "bd_latency_ms",
	store:  true,
}

// probes are the group's columns, in the order they run. They run one after
// another, so Collect costs at most one timeout per probe, which stays under
// the group's own deadline (SourceTimeout) as
// TestSourceHostProbesFitTheGroupDeadline holds it to.
func (s hostSource) probes() []hostProbe {
	return []hostProbe{
		{
			// The one-minute load average, so the record's seat counts are read
			// against the host they share (gt-b9j3o.3). One minute is the window
			// the snapshot's hour is about; the five- and fifteen-minute figures
			// wash it out.
			column: "load1",
			argv:   []string{"/usr/sbin/sysctl", "-n", "vm.loadavg"},
			parse:  parseLoadAvg1,
		},
		hostIdleProbe,
		// One bd list in the town's store, read twice: the first read is an
		// untimed warm-up of the seam, the second is the timed one.
		hostLatencyProbe,
		{
			column: "dolt_conns",
			argv:   []string{"lsof", "-nP", "-iTCP:" + s.doltPort, "-sTCP:ESTABLISHED"},
			parse:  parseDoltConns,
		},
		{
			column: "docker_disk_pct",
			argv: []string{
				"docker", "--context", "desktop-linux", "exec",
				s.dockerContainer, "df", "-Pk", "/",
			},
			parse: parseDockerDiskPct,
		},
	}
}

// read runs the probe's read under timeout and returns its column's value and
// whether it is a reading. A command that fails, misses its deadline, or prints
// nothing where an answer is the point, and output the parser cannot read, are
// all (nil, false) — and so is a store read that fails, since the seam only
// returns one that bd answered with JSON. Either way the column is a gap that
// Collect counts.
func (p hostProbe) read(ctx context.Context, env Env, timeout time.Duration) (any, bool) {
	if p.store {
		return p.readStore(ctx, env, timeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := env.Run(ctx, p.argv)
	if err != nil {
		return nil, false
	}
	value, err := p.parse(string(out))
	if err != nil {
		return nil, false
	}
	return value, true
}

// readStore times one list of one issue in the town's store: the seam's
// cheapest read of the board, so its time is what a bead read costs right now.
//
// The warm-up and the timed read get a deadline each and a store each, so a
// slow warm-up cannot spend the timed read's budget or leave it holding a
// finished context.
//
// The timed read is one bd invocation. The seam retries only a failure against
// its ephemeral test Dolt container (bd_container_retry.go, scoped to
// NewIsolatedWithPort), and a store built with beads.New is not that, so a
// connection-shaped failure ends the read rather than starting another:
// bd_latency_ms cannot hide a retry inside its window.
//
// The store is opened for the town root the snapshot names rather than the
// process's working directory the pre-seam `bd list --limit 1` ran in. The
// town-metrics plugin starts the snapshot in the town root, so this names the
// store that read reached; a snapshot started anywhere else now reads the town
// instead of whatever directory it happened to start in.
//
// The start is read before the timed read runs, so the difference is that
// read's own time. An empty board is an answer, so only a failed read is a
// gap; its fast return is not a latency.
func (hostProbe) readStore(ctx context.Context, env Env, timeout time.Duration) (any, bool) {
	if !warmStore(ctx, env, timeout) {
		return nil, false
	}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	store := env.Beads(readCtx, env.Town)
	if store == nil {
		return nil, false
	}
	start := env.Now()
	if _, err := store.List(beads.ListOptions{Limit: 1}); err != nil {
		return nil, false
	}
	return float64(env.Now().Sub(start)) / float64(time.Millisecond), true
}

// warmStore spends the seam's first-use setup before the clock starts. Its
// first call pays what a later one does not — the --allow-stale capability
// probe and the town's config reads — so timing it would report first use
// rather than what a bead read costs now. It costs one invocation per
// snapshot, and a read it cannot answer is a gap in bd_latency_ms exactly as a
// failed timed read is.
func warmStore(ctx context.Context, env Env, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	store := env.Beads(ctx, env.Town)
	if store == nil {
		return false
	}
	_, err := store.List(beads.ListOptions{Limit: 1})
	return err == nil
}

// parseLoadAvg1 reads the one-minute load average off sysctl's vm.loadavg,
// which prints "{ 1.23 4.56 7.89 }". The first of three numeric fields is the
// one-minute figure; a shorter or non-numeric line is not vm.loadavg's answer,
// so it is a missing reading rather than a wrong one. macOS counts runnable
// threads into this number, so it is read beside the idle share rather than as
// a saturation (internal/hoststat).
func parseLoadAvg1(out string) (any, error) {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(out), "{}"))
	if len(fields) < 3 {
		return nil, fmt.Errorf("vm.loadavg printed %d load fields, want 3", len(fields))
	}
	loads := make([]float64, 3)
	for i := range loads {
		load, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return nil, fmt.Errorf("vm.loadavg load field %d %q: %w", i+1, fields[i], err)
		}
		loads[i] = load
	}
	return loads[0], nil
}

// parseCPUIdlePct reads the idle share off top's last CPU line. A two-sample
// top prints one CPU line per sample, and the first averages the time since
// boot, so the last line is the window the snapshot is about.
func parseCPUIdlePct(out string) (any, error) {
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(strings.ToLower(l), "cpu usage") {
			line = l
		}
	}
	if line == "" {
		return nil, fmt.Errorf("top printed no CPU usage line")
	}
	fields := strings.Fields(line)
	for i, field := range fields {
		if i == 0 || strings.TrimSuffix(field, ",") != "idle" {
			continue
		}
		idle, err := strconv.ParseFloat(strings.Trim(fields[i-1], ",%"), 64)
		if err != nil {
			return nil, fmt.Errorf("top idle share %q: %w", fields[i-1], err)
		}
		return idle, nil
	}
	return nil, fmt.Errorf("top's CPU line names no idle share")
}

// parseDoltConns counts the established connections in lsof's output. lsof
// prints one line per endpoint, so a connection appears twice, once for each
// process holding it; the endpoints are ordered into one key so the pair
// counts once. Any line that is not a connection is an error, so a format
// change reads as missing rather than as zero connections.
func parseDoltConns(out string) (any, error) {
	text := strings.TrimSpace(out)
	if text == "" {
		return nil, fmt.Errorf("lsof printed nothing, not even its header")
	}
	conns := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "COMMAND") {
			continue
		}
		name := ""
		for _, field := range strings.Fields(line) {
			if strings.Contains(field, "->") {
				name = field
				break
			}
		}
		ends := strings.Split(name, "->")
		if len(ends) != 2 || !strings.Contains(line, "(ESTABLISHED)") {
			return nil, fmt.Errorf("lsof line names no established connection: %q", line)
		}
		if ends[0] > ends[1] {
			ends[0], ends[1] = ends[1], ends[0]
		}
		conns[ends[0]+"->"+ends[1]] = true
	}
	return len(conns), nil
}

// parseDockerDiskPct reads the used share of the filesystem mounted at / from
// df -Pk output: the capacity column of the root line.
func parseDockerDiskPct(out string) (any, error) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[len(fields)-1] != "/" {
			continue
		}
		capacity := fields[len(fields)-2]
		used, err := strconv.ParseFloat(strings.TrimSuffix(capacity, "%"), 64)
		if err != nil {
			return nil, fmt.Errorf("df capacity %q: %w", capacity, err)
		}
		return used, nil
	}
	return nil, fmt.Errorf("df printed no line for the / filesystem")
}
