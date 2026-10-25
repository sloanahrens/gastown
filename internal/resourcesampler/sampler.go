// Package resourcesampler samples the host's CPU and memory into the schema v1
// rows the town's metrics read (gt-bojte).
//
// This is the host half of scripts/resource-sampler.py in Go: one tick reads the
// process list, turns each process's accumulated CPU time into percent of one
// core since the previous tick, and appends one row per component to
// <metrics>/resources/<UTC date>.jsonl. The docker, kubectl and bead-peak
// sources, the daemon patrol and the script's deletion are later beads
// (gt-qqwey).
package resourcesampler

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// SchemaVersion is the row version this sampler writes and internal/metrics
	// reads; a reader of another version skips these rows.
	SchemaVersion = 1

	// DefaultInterval is the period a Sampler measures CPU over before it has a
	// tick of its own to divide by.
	DefaultInterval = 30 * time.Second

	rowsSubdir = "resources"
	sourceName = "host"
	// tsLayout is the schema's timestamp: UTC, second precision, trailing Z.
	tsLayout  = "2006-01-02T15:04:05Z"
	dayLayout = "2006-01-02"
)

// Row is one host row of the schema's 30 s records, in the schema's field order:
// encoding/json writes struct fields in order, so the samplers and the readers
// agree on the shape as well as the names.
type Row struct {
	V             int      `json:"v"`
	TS            string   `json:"ts"`
	IntervalS     float64  `json:"interval_s"`
	Source        string   `json:"source"`
	Class         string   `json:"class"`
	Component     string   `json:"component"`
	Name          string   `json:"name"`
	Rig           *string  `json:"rig"`
	Polecat       *string  `json:"polecat"`
	Bead          *string  `json:"bead"`
	CPUPct        *float64 `json:"cpu_pct"`
	MemBytes      int64    `json:"mem_bytes"`
	MemLimitBytes *int64   `json:"mem_limit_bytes"`
}

// Process is one host process in a tick's process list.
type Process struct {
	PID      int
	RSSBytes int64
	// CPUTime is the CPU time the process has accumulated, the whole of it and
	// not one tick's share: the delta between two ticks is what the sampler
	// measures.
	CPUTime time.Duration
	// Elapsed is how long the process has been running, which tells a reused pid
	// from a live one.
	Elapsed time.Duration
	// Command is the full command line, argv[0] first.
	Command string
}

// ProcessLister returns the host's current processes.
type ProcessLister interface {
	ListProcesses(ctx context.Context) ([]Process, error)
}

// Clock supplies the current time.
type Clock interface {
	Now() time.Time
}

// SystemClock is the Clock a running sampler uses.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// Sampler appends one row per host component on each tick.
type Sampler struct {
	metricsDir string
	processes  ProcessLister
	clock      Clock
	interval   time.Duration

	previous map[int]baseline
	lastTick time.Time
	started  bool
}

// New returns a Sampler that appends its rows under metricsDir, measuring each
// tick against the one before it.
func New(metricsDir string, processes ProcessLister, clock Clock, interval time.Duration) *Sampler {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Sampler{
		metricsDir: metricsDir,
		processes:  processes,
		clock:      clock,
		interval:   interval,
	}
}

// Tick samples the process list once and appends that tick's rows to
// <metricsDir>/resources/<UTC date>.jsonl, creating the directory and file when
// they are missing. A failed process list writes nothing and returns an error;
// the next tick reports null CPU, because the gap leaves no delta to divide.
func (s *Sampler) Tick(ctx context.Context) ([]Row, error) {
	now := s.clock.Now().UTC()
	interval := s.intervalAt(now)
	s.lastTick, s.started = now, true

	processes, err := s.processes.ListProcesses(ctx)
	if err != nil {
		// A gap leaves the next success no baseline it can trust: dividing two
		// ticks' CPU by one interval would invent a percentage. Dropping them
		// reports null CPU once instead.
		s.previous = nil
		return nil, fmt.Errorf("resource sampler: process list: %w", err)
	}
	rows := s.rows(processes, now, interval)
	if err := s.append(rows, now); err != nil {
		return rows, err
	}
	return rows, nil
}

// intervalAt returns the time since the previous tick, rounded to the tenth of
// a second interval_s carries. The first tick, and a clock that did not
// advance, use the configured interval.
func (s *Sampler) intervalAt(now time.Time) time.Duration {
	if s.started {
		if elapsed := now.Sub(s.lastTick); elapsed > 0 {
			return time.Duration(math.Round(elapsed.Seconds()*10)) * time.Second / 10
		}
	}
	return s.interval
}

// baseline is the previous tick's reading for one pid; the CPU delta and the
// reused-pid guard both read it.
type baseline struct {
	command string
	cpu     time.Duration
	elapsed time.Duration
}

// aggregate is one component's row while a tick is being summed.
type aggregate struct {
	cpu        float64
	measured   bool
	unmeasured int
	mem        int64
	name       string
	bestCPU    float64
	bestMem    int64
	named      bool
}

// add folds one process in. The row's name is the process with the highest CPU
// share, and on a tick with no share the one with the most memory; a tie on CPU
// keeps the larger memory, as the Python sampler's tuple comparison does. A
// process with no CPU delta is counted, so the component's cpu_pct can be null
// instead of a sum that leaves it out.
func (a *aggregate) add(p Process, name string, cpuPct float64, measured bool) {
	a.mem += p.RSSBytes
	marker := -1.0
	if measured {
		a.cpu += cpuPct
		a.measured = true
		marker = cpuPct
	} else {
		a.unmeasured++
	}
	if !a.named || marker > a.bestCPU || (marker == a.bestCPU && p.RSSBytes > a.bestMem) {
		a.name, a.bestCPU, a.bestMem, a.named = name, marker, p.RSSBytes, true
	}
}

// rows folds a tick's process list into one row per component, in component
// order, and keeps the tick's readings as the next tick's baseline.
func (s *Sampler) rows(processes []Process, now time.Time, interval time.Duration) []Row {
	current := make(map[int]baseline, len(processes))
	aggregates := make(map[string]*aggregate, len(processes))
	for _, p := range processes {
		ident := processName(p.Command)
		previous, seen := s.previous[p.PID]
		current[p.PID] = baseline{command: ident, cpu: p.CPUTime, elapsed: p.Elapsed}

		cpuPct, measured := percentOfCore(previous, seen, ident, p, interval)
		component := hostComponent(p.Command)
		agg := aggregates[component]
		if agg == nil {
			agg = &aggregate{}
			aggregates[component] = agg
		}
		agg.add(p, ident, cpuPct, measured)
	}
	s.previous = current

	components := make([]string, 0, len(aggregates))
	for component := range aggregates {
		components = append(components, component)
	}
	sort.Strings(components)

	rows := make([]Row, 0, len(components))
	for _, component := range components {
		agg := aggregates[component]
		row := Row{
			V:         SchemaVersion,
			TS:        now.Format(tsLayout),
			IntervalS: interval.Seconds(),
			Source:    sourceName,
			Class:     classOf(component),
			Component: component,
			Name:      agg.name,
			MemBytes:  agg.mem,
		}
		// cpu_pct is null unless every process in the component has a delta: a
		// sum over some of them would read low and say nothing about the rest.
		if agg.measured && agg.unmeasured == 0 {
			cpu := math.Round(agg.cpu*100) / 100
			row.CPUPct = &cpu
		}
		rows = append(rows, row)
	}
	return rows
}

// percentOfCore returns the CPU a process used since the previous tick as a
// share of one core. A pid whose elapsed time or CPU total went backwards was
// reused by a new process, so its totals are not a delta of the old one.
func percentOfCore(previous baseline, seen bool, ident string, p Process, interval time.Duration) (float64, bool) {
	if interval <= 0 || !seen || previous.command != ident {
		return 0, false
	}
	if p.Elapsed < previous.elapsed || p.CPUTime < previous.cpu {
		return 0, false
	}
	return float64(p.CPUTime-previous.cpu) / float64(interval) * 100, true
}

// append writes the rows as one JSON object per line, with the modes the
// metrics reader expects of the directory and a day file.
func (s *Sampler) append(rows []Row, now time.Time) error {
	if len(rows) == 0 {
		return nil
	}
	dir := filepath.Join(s.metricsDir, rowsSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("resource sampler: rows directory: %w", err)
	}
	path := filepath.Join(dir, now.Format(dayLayout)+".jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("resource sampler: rows file: %w", err)
	}
	defer file.Close()
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("resource sampler: encode row: %w", err)
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			return fmt.Errorf("resource sampler: append row: %w", err)
		}
	}
	return nil
}

// processName is the argv[0] basename, which a row names a process by.
func processName(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return filepath.Base(fields[0])
}

// componentClasses is the class each host component rolls up under. The strings
// are the schema's enum, so a component missing here would read as a new class.
var componentClasses = map[string]string{
	"dolt":      "control-plane",
	"gtbd":      "control-plane",
	"tests":     "ci",
	"claude":    "operator",
	"vm":        "operator",
	"av":        "operator",
	"spotlight": "operator",
	"kernel":    "operator",
	"other":     "other",
}

// classOf is the class a component rolls up under; a component with no entry is
// the schema's "other" rather than an empty class a reader would skip.
func classOf(component string) string {
	if class, ok := componentClasses[component]; ok {
		return class
	}
	return "other"
}

// hostComponent is the loadprof component of a host process, in the order the
// Python sampler tests it: a more specific binary wins over a broader path.
func hostComponent(command string) string {
	base := strings.ToLower(processName(command))
	low := strings.ToLower(command)
	switch {
	case strings.Contains(base, "dolt"):
		return "dolt"
	case base == "claude", strings.HasPrefix(base, "claude-"):
		return "claude"
	case (base == "node" || base == "nodejs") && strings.Contains(low, "claude"):
		return "claude"
	case base == "gt", base == "bd":
		return "gtbd"
	case isTestBinary(base), strings.Contains(low, "/go-build"), strings.Contains(low, "/go/pkg/tool/"):
		return "tests"
	case strings.HasPrefix(base, "kernel_task"):
		return "kernel"
	case containsAny(base, "mds", "mdworker", "spotlight"):
		return "spotlight"
	case containsAny(base, "xprotect", "falcon", "sentinel", "crowdstrike", "sophos", "clamd", "eset", "mcafee"):
		return "av"
	case containsAny(base, "qemu", "vmm", "vmnet", "hyperkit", "virtualiz", "lima"):
		return "vm"
	}
	return "other"
}

// isTestBinary reports whether a binary name is one a Go build or test runs.
func isTestBinary(base string) bool {
	switch base {
	case "go", "make", "compile", "link", "asm", "cgo", "vet":
		return true
	}
	return false
}

// containsAny reports whether s holds any of the tokens.
func containsAny(s string, tokens ...string) bool {
	for _, token := range tokens {
		if strings.Contains(s, token) {
			return true
		}
	}
	return false
}

// PSLister lists the host's processes with ps.
type PSLister struct{}

// psArgs asks ps for each process's pid, resident set size in KiB, accumulated
// CPU time, elapsed time and full command, with no header line.
var psArgs = []string{"-axo", "pid=,rss=,cputime=,etime=,command="}

// psLine splits ps's first four fields from the command, which holds spaces of
// its own; a line with fewer than five fields does not match.
var psLine = regexp.MustCompile(`^\s*(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S.*)$`)

// ListProcesses runs ps and parses its output.
func (PSLister) ListProcesses(ctx context.Context) ([]Process, error) {
	cmd := exec.CommandContext(ctx, "ps", psArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseProcessList(string(out))
}

// parseProcessList is ListProcesses's reading of ps output. Noise lines are
// skipped, but output that yields no process at all is an error: a tick that
// read garbage must not write an empty sample as if the machine were idle.
func parseProcessList(out string) ([]Process, error) {
	processes := parsePS(out)
	if len(processes) == 0 {
		return nil, fmt.Errorf("ps printed no process the sampler could read (%d bytes)", len(out))
	}
	return processes, nil
}

// parsePS parses the output psArgs asks for, skipping a line whose fields do
// not parse: ps's own noise must not end a tick.
func parsePS(out string) []Process {
	processes := make([]Process, 0, strings.Count(out, "\n")+1)
	for line := range strings.SplitSeq(out, "\n") {
		match := psLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		pid, ok := parsePID(match[1])
		if !ok {
			continue
		}
		rssKiB, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			continue
		}
		cpu, ok := parsePSTime(match[3])
		if !ok {
			continue
		}
		elapsed, ok := parsePSTime(match[4])
		if !ok {
			continue
		}
		processes = append(processes, Process{
			PID:      pid,
			RSSBytes: rssKiB * 1024,
			CPUTime:  cpu,
			Elapsed:  elapsed,
			Command:  match[5],
		})
	}
	return processes
}

// parsePID parses a ps pid; a sign or any other character is not a pid.
func parsePID(text string) (int, bool) {
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	pid, err := strconv.Atoi(text)
	return pid, err == nil
}

// parsePSTime parses a ps time field, [[dd-]hh:]mm:ss[.frac], into a duration.
func parsePSTime(text string) (time.Duration, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, false
	}
	days := 0
	if head, rest, found := strings.Cut(text, "-"); found {
		n, err := strconv.Atoi(head)
		if err != nil {
			return 0, false
		}
		days, text = n, rest
	}
	parts := strings.Split(text, ":")
	if len(parts) > 3 {
		return 0, false
	}
	// ps prints hh:mm:ss, but a short mm:ss means the same fields with no hours.
	seconds := make([]float64, 3)
	for i, part := range parts {
		value, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, false
		}
		seconds[3-len(parts)+i] = value
	}
	total := float64(days)*86400 + seconds[0]*3600 + seconds[1]*60 + seconds[2]
	return time.Duration(total * float64(time.Second)), true
}
