package metrics

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// hostCommandTimeout bounds each host probe separately, so a wedged top,
// sysctl, bead read, lsof or docker costs its own column and leaves the others
// filled.
const hostCommandTimeout = 5 * time.Second

// hostSource is the host group: the machine the snapshot runs on, read with
// the macOS commands the operator uses by hand, plus one timed bead read for
// what bd costs it. A probe that cannot give a reading leaves its column nil
// rather than inventing a number, and the failure is counted in
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
	// so df inside it reads the usage Docker Desktop fills (gt-mexpf). It is
	// the runner service of the town's compose stack, whose name comes from the
	// compose file; if that name changes, docker_disk_pct goes nil and
	// host_probe_failures says so until this default follows it.
	dockerContainer string
}

// newHostSource is the host group as the registry runs it: the default Dolt
// port and the Forgejo runner, which is part of the stack a town keeps up.
func newHostSource() hostSource {
	return hostSource{
		doltPort:        strconv.Itoa(doltserver.DefaultPort),
		dockerContainer: "forgejo-runner-daemon-1",
	}
}

func (hostSource) Group() string { return "host" }

// Collect fills the host group. A probe that cannot give a reading is a nil
// column counted in host_probe_failures; only an Env with no bead store, which
// is a wiring error rather than a host that could not answer, is an error.
func (s hostSource) Collect(ctx context.Context, env Env) (map[string]any, error) {
	if env.Beads == nil {
		return nil, fmt.Errorf("host: the environment has no bead store")
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

// hostProbe is one host column: the command that reads it and the parser that
// turns its stdout into the value. store marks the probe whose read is the
// issue store rather than a command: its value is the time the read took.
type hostProbe struct {
	column string
	argv   []string
	store  bool
	parse  func(string) (any, error)
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
		{
			column: "cpu_idle_pct",
			argv:   []string{"/usr/bin/top", "-l", "2", "-n", "0", "-s", "1"},
			parse:  parseCPUIdlePct,
		},
		{
			// One bd list in the town's store, timed: the seam's machine read
			// of one issue is the cheapest call the town itself makes, so its
			// time is what a bead read costs right now.
			column: "bd_latency_ms",
			store:  true,
		},
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
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if p.store {
		// Warm the seam before the clock starts. Its first call pays setup a
		// later call does not — the --allow-stale capability probe and the
		// town's config reads — so timing it would report first use rather
		// than what a bead read costs now. The seam's container retry never
		// applies to a town snapshot, so the timed call is one bd invocation.
		//
		// The start is read before the timed read runs, so the difference is
		// that read's own time. An empty board is an answer, so only a failed
		// read is a gap; its fast return is not a latency.
		store := env.Beads(ctx, env.Town)
		if store == nil {
			return nil, false
		}
		if _, err := store.List(beads.ListOptions{Limit: 1}); err != nil {
			return nil, false
		}
		start := env.Now()
		if _, err := store.List(beads.ListOptions{Limit: 1}); err != nil {
			return nil, false
		}
		return float64(env.Now().Sub(start)) / float64(time.Millisecond), true
	}
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
