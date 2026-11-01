package hostload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonlog"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

const (
	// SampleWindow is the interval a sample's CPU reading covers: what each
	// process used over this window, not its average since it started.
	SampleWindow = time.Second
	// DefaultEvery is the interval `gt host load --sample` appends at unless
	// the operator sets another.
	DefaultEvery = 10 * time.Second
	// cmdTimeout bounds one host command.
	cmdTimeout = 30 * time.Second
	// bdTimeout bounds the one bd call a sample times.
	bdTimeout = 10 * time.Second
	// tickWindow is how far back the dispatcher's last tick is looked for:
	// ten intervals of its 60s default.
	tickWindow = 10 * time.Minute
	// logTailBytes is how much of daemon.log is read for the tick. The tail
	// holds many ticks, so a log far larger than this still answers.
	logTailBytes = 256 << 10
	// lsofChunk is how many pids one lsof call names. lsof is asked about the
	// pids that used CPU and no others, so the probe never walks the host's
	// process table.
	lsofChunk = 200
	// procLoadavg is the kernel's load counters where the kernel keeps them.
	procLoadavg = "/proc/loadavg"
)

// The commands a sample runs. topArgv takes two samples so it reports the CPU
// used over the window rather than the average since each process started;
// psArgv carries the command lines top cuts short.
var (
	sysctlLoadArgv = []string{"sysctl", "-n", "vm.loadavg"}
	psArgv         = []string{"ps", "-axo", "pid=,command="}
	bdArgv         = []string{"bd", "list", "--limit", "1"}
)

// errNo* are the errors a Prober without its seam reports instead of running
// or reading nothing and reporting a quiet host.
var (
	errNoRunner = errors.New("hostload: no command runner configured")
	errNoSeats  = errors.New("hostload: no seat reader configured")
	errNoLog    = errors.New("hostload: no log reader configured")
)

// Load is the host's three load averages.
type Load struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// Sample is one reading of the host: what `gt host load` prints and what
// `--sample` appends as one JSON line. A probe that failed carries its error in
// its own Field and leaves the rest of the sample intact.
type Sample struct {
	At          time.Time                `json:"at"`
	Load        hostprobe.Field[Load]    `json:"load"`
	BDLatencyMS hostprobe.Field[float64] `json:"bd_latency_ms"`
	TickMS      hostprobe.Field[float64] `json:"tick_ms"`
	Seats       hostprobe.Field[int]     `json:"seats"`
	Owners      hostprobe.Field[[]Row]   `json:"owners"`
}

// Prober is the host a Sample reads. NewProber wires the real seams; a test
// sets the ones it scripts and leaves the rest nil, and a read whose seam is
// nil reports an error instead of an empty reading.
type Prober struct {
	TownRoot string
	// Run runs one host command and returns its stdout. Its error must not
	// carry the command's output: the output is a reading, and an error that
	// echoed it would put the host's process table in a record.
	Run hostprobe.Runner
	// ReadFile reads the kernel's load counters; nil means os.ReadFile.
	ReadFile func(path string) ([]byte, error)
	// SeatCount returns the town's seat capacity; nil means the town settings.
	SeatCount func() (int, error)
	// ReadLog reads the last n bytes of a file; nil means readTail. It reads
	// the end of daemon.log, so a large log costs its tail and not its size.
	ReadLog func(path string, n int64) ([]byte, error)
	// Clock stamps a sample; nil means the real clock.
	Clock clockwork.Clock
	// LogDir holds daemon.log; "" means <TownRoot>/daemon.
	LogDir string
	// Loc reads daemon.log's timestamps; nil means time.Local.
	Loc *time.Location
}

// NewProber is a Prober on this host, reporting on townRoot.
func NewProber(townRoot string) *Prober {
	p := &Prober{
		TownRoot: townRoot,
		Run: func(ctx context.Context, argv []string) ([]byte, error) {
			return runArgv(ctx, townRoot, argv)
		},
		ReadFile: os.ReadFile,
		ReadLog:  readTail,
		SeatCount: func() (int, error) {
			return townSeatCount(townRoot)
		},
	}
	return p
}

// Sample reads the host once. Each probe is its own read: one that fails sets
// its own field's error and the probes after it still run.
func (p *Prober) Sample(ctx context.Context) Sample {
	now := p.clock().Now()
	owners, err := p.owners(ctx)
	return Sample{
		At:          now,
		Load:        fieldOf(p.load(ctx)),
		BDLatencyMS: p.bdLatency(ctx),
		TickMS:      p.tick(now),
		Seats:       p.seats(),
		Owners:      fieldOf(owners, err),
	}
}

func (p *Prober) clock() clockwork.Clock {
	if p.Clock != nil {
		return p.Clock
	}
	return clockwork.NewRealClock()
}

// runBounded runs one command under its own deadline, so a hung probe fails
// its own field and not the sample.
func (p *Prober) runBounded(ctx context.Context, bound time.Duration, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	return p.Run(ctx, argv)
}

// owners reads the process table, the CPU each process used and the working
// directory each runs in, and returns CPU by owner. A read that fails outright
// is the owners field's error: a table built with no working directories would
// charge every crew session to its programs and call the result complete.
func (p *Prober) owners(ctx context.Context) ([]Row, error) {
	if p.Run == nil {
		return nil, errNoRunner
	}
	psOut, err := p.runBounded(ctx, cmdTimeout, psArgv)
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	topOut, err := p.runBounded(ctx, cmdTimeout, topArgv())
	if err != nil {
		return nil, fmt.Errorf("top: %w", err)
	}
	cpu, err := ParseTop(string(topOut))
	if err != nil {
		return nil, err
	}
	lsofOut, err := p.cwds(ctx, cpu)
	if err != nil {
		return nil, err
	}
	return Table(string(psOut), string(topOut), lsofOut)
}

// cwds asks lsof for the working directory of every process that used CPU,
// a chunk of pids at a time, and returns the answers as one -Fpn stream.
// lsof exits non-zero when any pid it was asked about has gone, while still
// answering for the rest, so a chunk's answer is kept even when its call
// failed (gt-nu89c); only a call that answered nothing fails the read.
func (p *Prober) cwds(ctx context.Context, cpu map[int]float64) (string, error) {
	var pids []int
	for pid, pct := range cpu {
		if pct > 0 {
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		return "", nil
	}
	sort.Ints(pids)
	var out strings.Builder
	for i := 0; i < len(pids); i += lsofChunk {
		chunk := pids[i:min(i+lsofChunk, len(pids))]
		argv := append([]string{"lsof", "-a", "-d", "cwd", "-Fpn", "-p"}, joinInts(chunk))
		got, err := p.runBounded(ctx, cmdTimeout, argv)
		if err != nil && len(got) == 0 {
			return "", fmt.Errorf("lsof: %w", err)
		}
		out.Write(got)
		out.WriteByte('\n')
	}
	return out.String(), nil
}

// tick reads the dispatcher's last tick duration from the end of daemon.log.
// A tick older than tickWindow is not reported: its length says nothing about
// the host now.
func (p *Prober) tick(now time.Time) hostprobe.Field[float64] {
	if p.ReadLog == nil {
		return hostprobe.Field[float64]{Err: errNoLog.Error()}
	}
	dir := p.LogDir
	if dir == "" {
		dir = filepath.Join(p.TownRoot, "daemon")
	}
	path := filepath.Join(dir, daemonlog.FileName)
	data, err := p.ReadLog(path, logTailBytes)
	if err != nil {
		return hostprobe.Field[float64]{Err: fmt.Sprintf("daemon.log: %v", err)}
	}
	loc := p.Loc
	if loc == nil {
		loc = time.Local
	}
	parser := &daemonlog.Parser{Loc: loc}
	cutoff := now.Add(-tickWindow)
	var (
		took  float64
		found bool
	)
	for _, raw := range strings.Split(string(data), "\n") {
		at, text := parser.Parse(raw)
		if at.Before(cutoff) {
			continue
		}
		if ms, ok := parseTickTook(text); ok {
			took, found = ms, true
		}
	}
	if !found {
		return hostprobe.Field[float64]{Err: fmt.Sprintf("no dispatch tick in %s in the last %s", path, tickWindow)}
	}
	return hostprobe.Field[float64]{Value: took}
}

// parseTickTook reads the milliseconds off a daemon line that says "tick took
// 1.234s".
func parseTickTook(text string) (float64, bool) {
	const marker = "tick took "
	i := strings.Index(text, marker)
	if i < 0 {
		return 0, false
	}
	rest := text[i+len(marker):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	d, err := time.ParseDuration(rest)
	if err != nil {
		return 0, false
	}
	return float64(d.Microseconds()) / 1000, true
}

// load reads the host's load averages, the secondary number hoststat says
// macOS inflates with runnable threads.
func (p *Prober) load(ctx context.Context) (Load, error) {
	readErr := errors.New("/proc/loadavg: no file reader configured")
	if p.ReadFile != nil {
		data, err := p.ReadFile(procLoadavg)
		if err != nil {
			readErr = fmt.Errorf("/proc/loadavg: %w", err)
		} else if l, err := parseLoad(strings.Fields(string(data))); err != nil {
			readErr = fmt.Errorf("/proc/loadavg: %w", err)
		} else {
			return l, nil
		}
	}
	if p.Run == nil {
		return Load{}, readErr
	}
	out, err := p.runBounded(ctx, cmdTimeout, sysctlLoadArgv)
	if err != nil {
		return Load{}, fmt.Errorf("%v; sysctl -n vm.loadavg: %w", readErr, err)
	}
	return parseLoad(strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}")))
}

// parseLoad reads three load averages off the front of a field list.
func parseLoad(f []string) (Load, error) {
	if len(f) < 3 {
		return Load{}, fmt.Errorf("%d load averages, want 3", len(f))
	}
	var v [3]float64
	for i := range v {
		x, err := strconv.ParseFloat(strings.TrimRight(f[i], ","), 64)
		if err != nil || x < 0 || math.IsNaN(x) || math.IsInf(x, 0) {
			return Load{}, errors.New("a load average is not a number")
		}
		v[i] = x
	}
	return Load{One: v[0], Five: v[1], Fifteen: v[2]}, nil
}

// bdLatency times one bd call: the database's answer time is part of what
// makes the dispatcher's tick slow, and it is not a CPU number.
func (p *Prober) bdLatency(ctx context.Context) hostprobe.Field[float64] {
	if p.Run == nil {
		return hostprobe.Field[float64]{Err: errNoRunner.Error()}
	}
	clk := p.clock()
	start := clk.Now()
	_, err := p.runBounded(ctx, bdTimeout, bdArgv)
	elapsed := clk.Now().Sub(start)
	if err != nil {
		return hostprobe.Field[float64]{Err: fmt.Sprintf("bd list --limit 1: %v", err)}
	}
	return hostprobe.Field[float64]{Value: float64(elapsed.Microseconds()) / 1000}
}

// seats reads the town's seat capacity.
func (p *Prober) seats() hostprobe.Field[int] {
	if p.SeatCount == nil {
		return hostprobe.Field[int]{Err: errNoSeats.Error()}
	}
	return fieldOf(p.SeatCount())
}

// fieldOf is one reading as a Field: its value when the read succeeded, its
// error text when it did not.
func fieldOf[T any](v T, err error) hostprobe.Field[T] {
	if err != nil {
		return hostprobe.Field[T]{Err: err.Error()}
	}
	return hostprobe.Field[T]{Value: v}
}

// topArgv is the two-sample top a sample reads CPU from, at SampleWindow's
// interval.
func topArgv() []string {
	secs := strconv.Itoa(int(SampleWindow / time.Second))
	if runtime.GOOS == "darwin" {
		return []string{"top", "-l", "2", "-s", secs, "-o", "cpu", "-stats", "pid,cpu"}
	}
	return []string{"top", "-b", "-n", "2", "-d", secs, "-w", "512"}
}

// joinInts renders pids as the comma-separated list lsof's -p takes.
func joinInts(pids []int) string {
	var b strings.Builder
	for i, pid := range pids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(pid))
	}
	return b.String()
}

// runArgv runs argv in dir and returns its stdout. The error is the command's
// own and never its output, which the Sample record may carry.
func runArgv(ctx context.Context, dir string, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command to run")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is built in this package
	cmd.Dir = dir
	return cmd.Output()
}

// tailReader is what a tail read needs of an open log: *os.File in production,
// and a fake in a test, since a file on disk cannot be made to fail Stat or
// ReadAt on demand.
type tailReader interface {
	Stat() (os.FileInfo, error)
	ReadAt(p []byte, off int64) (int, error)
}

// readTail returns the last n bytes of path, dropping a partial first line so
// every line returned is whole.
func readTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return tail(f, n)
}

// tail returns the last n bytes of f, dropping a partial first line so every
// line returned is whole. A tail that is one partial line returns no lines.
func tail(f tailReader, n int64) ([]byte, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	off := max(size-n, 0)
	// Read one byte before the bound when it cuts into the file, so the read
	// can tell a line boundary from the middle of a line.
	if off > 0 {
		off--
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	if off == 0 {
		return buf, nil
	}
	if buf[0] == '\n' {
		return buf[1:], nil
	}
	i := bytes.IndexByte(buf, '\n')
	if i < 0 {
		return nil, nil
	}
	return buf[i+1:], nil
}

// townSeatCount reads the town's seat capacity from its settings. A town with
// no settings file reports an error: an unread file is not a reading of zero
// seats.
func townSeatCount(townRoot string) (int, error) {
	path := config.TownSettingsPath(townRoot)
	ts, err := config.LoadTownSettings(path)
	if err != nil {
		return 0, fmt.Errorf("town settings: %w", err)
	}
	if ts == nil {
		return 0, fmt.Errorf("town settings: no file at %s", path)
	}
	return ts.PolecatPool.SeatTotal(), nil
}
