package metrics

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// hostCommandTimeout bounds each host probe separately, so a wedged top, bd,
// lsof or docker costs its own column and leaves the other three filled.
const hostCommandTimeout = 5 * time.Second

// hostSource is the host group: the machine the snapshot runs on, read with
// the macOS commands the operator uses by hand. A host without one of them,
// or one whose command fails or misses hostCommandTimeout, leaves that column
// nil rather than inventing a number.
type hostSource struct {
	// doltPort is the port the Dolt server listens on. lsof counts the
	// connections to it, which answers without a database round trip, so a
	// server too busy to answer still reports its connection count.
	doltPort string
	// dockerContainer is a container whose filesystem is the Docker VM's disk,
	// so df inside it reads the usage Docker Desktop fills (gt-mexpf).
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

// Collect fills the host group. Output a parser cannot read is an error, which
// nulls the group: a fixture that no longer matches the command must show up
// as missing rather than as a wrong reading.
func (s hostSource) Collect(ctx context.Context, env Env) (map[string]any, error) {
	probes := s.probes()
	cols := make(map[string]any, len(probes))
	for _, probe := range probes {
		value, err := probe.read(ctx, env)
		if err != nil {
			return nil, fmt.Errorf("host %s: %w", probe.column, err)
		}
		cols[probe.column] = value
	}
	return cols, nil
}

// hostProbe is one host column: the command that reads it and the parser that
// turns its stdout into the value. latency marks a probe whose value is the
// time its command took rather than anything it printed.
type hostProbe struct {
	column  string
	argv    []string
	latency bool
	parse   func(string) (any, error)
}

// probes are the group's columns, in the order they run. They run one after
// another, so Collect costs at most four timeouts.
func (s hostSource) probes() []hostProbe {
	return []hostProbe{
		{
			column: "cpu_idle_pct",
			argv:   []string{"/usr/bin/top", "-l", "2", "-n", "0", "-s", "1"},
			parse:  parseCPUIdlePct,
		},
		{
			column:  "bd_latency_ms",
			argv:    []string{"bd", "list", "--limit", "1"},
			latency: true,
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

// read runs the probe's command and returns its column's value. A command
// that fails or misses its deadline is a nil value and no error, the gap a
// host that cannot answer leaves in the record; a parser error is returned,
// because that is a bug in the source rather than a reading the host refused.
func (p hostProbe) read(ctx context.Context, env Env) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, hostCommandTimeout)
	defer cancel()
	if p.latency {
		// The start is read before the command runs, so the difference is the
		// command's own time. An empty answer is nil: a bd that printed nothing
		// did not answer, and its fast return is not a latency.
		start := env.Now()
		out, err := env.Run(ctx, p.argv)
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return nil, nil
		}
		return float64(env.Now().Sub(start)) / float64(time.Millisecond), nil
	}
	out, err := env.Run(ctx, p.argv)
	if err != nil {
		return nil, nil
	}
	return p.parse(string(out))
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
