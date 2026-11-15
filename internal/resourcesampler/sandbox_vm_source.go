package resourcesampler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/workspaceid"
)

// The VM row's schema values (design row table): the sandbox cluster's source
// and class, the vm component, and the one name both the row and its cluster
// carry. rig, polecat and bead are null on a VM row, and so is mem_limit_bytes.
const (
	vmSource    = seatSource
	vmClass     = seatClass
	vmComponent = "vm"
	vmName      = SandboxCluster
)

// vmProcFiles are the proc files one VM read prints, in the order a read prints
// them. stat and meminfo are the reading itself, so an empty section there is a
// refused read; loadavg and the two pressure files are null when their section
// carries no text, which is how a read prints a file the kernel does not
// publish. The names are the paths under /proc, so a read's markers name the
// file it printed.
var vmProcFiles = []string{"loadavg", "pressure/cpu", "pressure/memory", "stat", "meminfo"}

// VMReading is the gt-sbx VM's /proc/stat reading at one sample: the baseline
// the next row's cpu_pct is measured from. Cores is kept with the counters
// because a CPU count that changed between two samples makes them
// incomparable: percent of one core is scaled by the core count.
type VMReading struct {
	Cores uint64
	Busy  uint64
	Total uint64
	At    time.Time
}

// VMRow builds the one schema v1 row the gt-sbx VM's proc text describes, and
// returns the reading to pass as previous on the next sample (gt-9y0kii).
// previous is nil for the first sample, whose cpu_pct is null: /proc/stat
// carries a counter, not a share, so the first reading is a baseline and not a
// measurement.
//
// The text is what one read of the VM's proc files prints, by the same marker
// convention the seat read uses: for each file in vmProcFiles, a line naming it
// between "@@ " and " @@", then its contents, and a closing "@@ end @@" marker.
// A file the kernel does not publish is printed as its marker with no text
// after it, so every file keeps its section. An error, not a row, is what a
// text that does not read as a VM's proc files produces: a refused reading
// writes no row rather than one of zeros.
func VMRow(text string, now time.Time, previous *VMReading, workspaces *workspaceid.Map) (Row, VMReading, error) {
	if workspaces == nil {
		return Row{}, VMReading{}, errors.New("sandbox vm source: no workspace map")
	}
	proc, err := parseVMProc(text)
	if err != nil {
		return Row{}, VMReading{}, fmt.Errorf("sandbox vm source: %w", err)
	}

	reading := VMReading{Cores: proc.cores, Busy: proc.busy, Total: proc.total, At: now}
	interval, cpuPct := vmCPU(reading, previous)
	cluster := vmName
	row := Row{
		V:                  SchemaVersion,
		TS:                 now.UTC().Format(tsLayout),
		IntervalS:          interval,
		Source:             vmSource,
		Class:              vmClass,
		Component:          vmComponent,
		Name:               vmName,
		CPUPct:             cpuPct,
		MemBytes:           proc.memBytes,
		Cluster:            &cluster,
		PressureCPUSomePct: proc.cpuSome,
		PressureMemSomePct: proc.memSome,
		PressureMemFullPct: proc.memFull,
		Load1:              proc.load1,
	}
	row.SetWorkspace(workspaces)
	return row, reading, nil
}

// vmProc is one VM read's parsed readings; a nil reading is one a section the
// kernel did not publish left null.
type vmProc struct {
	load1    *float64
	cpuSome  *float64
	memSome  *float64
	memFull  *float64
	cores    uint64
	busy     uint64
	total    uint64
	memBytes int64
}

// vmCPU returns the interval_s and cpu_pct of reading measured against the
// VM's previous one. The first sample has no previous reading to measure from,
// so it reports interval_s 0 with null CPU. A core count that changed, a
// counter that went backwards and a total that did not advance are readings
// the previous one cannot be subtracted from, so they report the interval with
// null CPU; dividing by a zero delta would invent a percentage.
func vmCPU(reading VMReading, previous *VMReading) (float64, *float64) {
	if previous == nil {
		return 0, nil
	}
	interval := time.Duration(math.Round(reading.At.Sub(previous.At).Seconds()*10)) * time.Second / 10
	if interval <= 0 {
		return 0, nil
	}
	if reading.Cores != previous.Cores || reading.Total <= previous.Total || reading.Busy < previous.Busy {
		return interval.Seconds(), nil
	}
	busy := float64(reading.Busy - previous.Busy)
	total := float64(reading.Total - previous.Total)
	// The aggregate cpu line sums every core's ticks, so the busy share of all
	// cores scaled by the core count is the share of one core: a VM whose four
	// cores are all busy reports 400.
	pct := math.Round(busy/total*float64(reading.Cores)*100*100) / 100
	return interval.Seconds(), &pct
}

// VMTick is one tick's VM sample: the row of the first live seat whose read
// produced one, and one skip per seat the read could not be taken through. A
// tick with no live seat and a registry whose every record is corrupt both
// carry no row; the skips tell them apart.
type VMTick struct {
	Row     *Row
	Skipped []SeatSkip
}

// skip records a seat the VM read wrote no row through.
func (t *VMTick) skip(seat string, err error) {
	t.Skipped = append(t.Skipped, SeatSkip{Seat: seat, Err: err})
}

// VMSource reads the five proc files the VM row is built from through one live
// seat: it tries the live seats in name order until one read produces the row
// (gt-2qe2m6). It keeps the last reading that wrote a row, so cpu_pct is the
// delta over the VM's previous sample. Tick is not safe for concurrent use: the
// patrol ticks it serially.
type VMSource struct {
	townRoot string
	runner   SeatExec
	clock    Clock
	timeout  time.Duration
	// previous is the VM's last reading that wrote a row. A tick whose every
	// read failed keeps it, so the next row is measured over the whole gap.
	previous *VMReading
}

// NewVMSource returns the source the patrol ticks: one live seat's proc files
// read through runner, each read bounded by timeout, sampled at clock's time.
func NewVMSource(townRoot string, runner SeatExec, clock Clock, timeout time.Duration) *VMSource {
	if clock == nil {
		clock = SystemClock{}
	}
	if timeout <= 0 {
		timeout = DefaultSeatTimeout
	}
	return &VMSource{townRoot: townRoot, runner: runner, clock: clock, timeout: timeout}
}

// Tick reads the VM's proc files through the first live seat that answers and
// returns its row. A seat whose record cannot be read, whose name an exec must
// not address, or whose read fails is skipped and the next live seat is tried,
// so one stale record cannot keep the VM row out of every tick. A tick no seat
// answers writes no row, and the caller writes no VM row rather than one of
// zeros.
//
// A failure of the whole source — no exec seam, a workspace map or a registry
// directory that cannot be read — is an error and no row.
func (s *VMSource) Tick(ctx context.Context) (VMTick, error) {
	if s.runner == nil {
		return VMTick{}, errors.New("sandbox vm source: no exec seam")
	}
	// The map is read before any seat, so a bad map costs no exec and no row,
	// the rule the seat source applies.
	workspaces, err := workspaceid.Load(s.townRoot)
	if err != nil {
		return VMTick{}, fmt.Errorf("sandbox vm source: %w", err)
	}
	records, err := seatRecords(s.townRoot)
	if err != nil {
		return VMTick{}, fmt.Errorf("sandbox vm source: %w", err)
	}
	now := s.clock.Now()
	argv := vmProcReadArgv()
	var tick VMTick
	for _, record := range records {
		if record.err != nil {
			tick.skip(record.name, record.err)
			continue
		}
		// !found with no error: the record was removed between the listing and
		// the read, so the seat has ended and another one answers.
		if !record.found {
			continue
		}
		seat, err := seatFromRecord(record.rec)
		if err != nil {
			tick.skip(record.name, err)
			continue
		}
		text, err := s.readProc(ctx, seat.Name, argv)
		if err != nil {
			tick.skip(seat.Name, err)
			continue
		}
		row, reading, err := VMRow(text, now, s.previous, workspaces)
		if err != nil {
			tick.skip(seat.Name, err)
			continue
		}
		s.previous = &reading
		tick.Row = &row
		return tick, nil
	}
	return tick, nil
}

// readProc runs one seat's proc read under the source's per-seat bound, so a
// wedged gateway call costs the VM row and not the tick. The name has already
// been validated by seatFromRecord: it reaches the seam as an argument of the
// exec and nowhere else.
func (s *VMSource) readProc(ctx context.Context, name string, argv []string) (string, error) {
	procCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	stdout, _, err := s.runner.Exec(procCtx, name, nil, argv, nil)
	if err != nil {
		return "", fmt.Errorf("vm read in seat %s: %w", name, err)
	}
	return string(stdout), nil
}

// vmProcReadArgv is the argv one VM read runs: a shell that prints each of the
// VM's proc files under the marker line VMRow's parser reads it by, then the
// end marker. A file the kernel does not publish prints its marker and nothing
// else, which parses as null where the file is optional and as a refused read
// where it is the reading itself. The print after each file keeps a text
// without a trailing newline off the next marker.
func vmProcReadArgv() []string {
	var script strings.Builder
	for _, name := range vmProcFiles {
		fmt.Fprintf(&script, "printf '@@ %s @@\\n'\ncat /proc/%s 2>/dev/null\nprintf '\\n'\n", name, name)
	}
	script.WriteString("printf '@@ end @@\\n'\n")
	return []string{"sh", "-c", script.String()}
}

// parseVMProc reads a VM read's text into its readings.
func parseVMProc(text string) (vmProc, error) {
	sections, err := splitVMProc(text)
	if err != nil {
		return vmProc{}, err
	}
	var proc vmProc
	if proc.load1, err = parseVMLoad(sections["loadavg"]); err != nil {
		return vmProc{}, err
	}
	if proc.cpuSome, err = parsePressure("pressure/cpu", "some", sections["pressure/cpu"]); err != nil {
		return vmProc{}, err
	}
	if proc.memSome, err = parsePressure("pressure/memory", "some", sections["pressure/memory"]); err != nil {
		return vmProc{}, err
	}
	if proc.memFull, err = parsePressure("pressure/memory", "full", sections["pressure/memory"]); err != nil {
		return vmProc{}, err
	}
	if proc.cores, proc.busy, proc.total, err = parseVMStat(sections["stat"]); err != nil {
		return vmProc{}, err
	}
	if proc.memBytes, err = parseVMMeminfo(sections["meminfo"]); err != nil {
		return vmProc{}, err
	}
	return proc, nil
}

// splitVMProc splits a VM read into its files' texts. Every file in vmProcFiles
// must appear once, the end marker must close the text, and nothing may stand
// outside a section: a read that does not have that shape is not a VM's proc
// text and is refused.
func splitVMProc(text string) (map[string]string, error) {
	sections := make(map[string]string, len(vmProcFiles))
	var current string
	var body []string
	ended := false
	flush := func() {
		if current != "" {
			sections[current] = strings.Join(body, "\n")
			current, body = "", nil
		}
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		match := seatCgroupMarker.FindStringSubmatch(line)
		if match == nil {
			switch {
			case ended && strings.TrimSpace(line) != "":
				return nil, errors.New("the read continues after its end marker")
			case ended:
			case current == "" && strings.TrimSpace(line) != "":
				return nil, errors.New("the read starts before any file marker")
			case current == "":
			default:
				body = append(body, line)
			}
			continue
		}
		name := match[1]
		if ended {
			return nil, fmt.Errorf("the read prints %s after its end marker", shorten(name))
		}
		if name == "end" {
			flush()
			ended = true
			continue
		}
		if !slices.Contains(vmProcFiles, name) {
			return nil, fmt.Errorf("the read prints %s, which is not one of the VM's proc files", shorten(name))
		}
		flush()
		if _, duplicate := sections[name]; duplicate {
			return nil, fmt.Errorf("the read prints %s twice", shorten(name))
		}
		current, body = name, nil
	}
	flush()
	if !ended {
		return nil, errors.New("the read ends before its end marker")
	}
	for _, name := range vmProcFiles {
		if _, ok := sections[name]; !ok {
			return nil, fmt.Errorf("the read prints no %s", name)
		}
	}
	return sections, nil
}

// parseVMLoad reads /proc/loadavg's first field, the one-minute load. An empty
// text is a file the kernel does not publish, which is null; a text whose first
// field is not a finite load is an error, so a damaged reading is never
// reported as an absent one.
func parseVMLoad(text string) (*float64, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil, nil
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || load < 0 || math.IsNaN(load) || math.IsInf(load, 0) {
		return nil, fmt.Errorf("loadavg's first field %s is not a load", shorten(fields[0]))
	}
	return &load, nil
}

// parseVMStat reads /proc/stat into the VM's core count and its aggregate
// busy and total ticks. The aggregate "cpu" line sums every core, so its total
// is the whole machine's ticks; busy is that total minus idle and iowait. The
// guest and guest_nice fields are left out of the total because the kernel
// already counts them in user and nice. Both the cpu line and at least one
// per-cpu line are required: without the core count a percent of one core
// cannot be computed, and the reading is the row's cpu_pct.
func parseVMStat(text string) (cores, busy, total uint64, err error) {
	var aggregate []string
	for _, raw := range strings.Split(text, "\n") {
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "cpu" {
			aggregate = fields[1:]
			continue
		}
		if isCPULine(fields[0]) {
			cores++
		}
	}
	if aggregate == nil {
		return 0, 0, 0, errors.New("stat carries no cpu line")
	}
	if cores == 0 {
		return 0, 0, 0, errors.New("stat names no cpu")
	}
	if len(aggregate) < 4 {
		return 0, 0, 0, errors.New("the cpu line carries no idle")
	}
	ticks := make([]uint64, len(aggregate))
	for i, field := range aggregate {
		if ticks[i], err = strconv.ParseUint(field, 10, 64); err != nil {
			return 0, 0, 0, fmt.Errorf("the cpu line's field %s is not a tick count", shorten(field))
		}
	}
	for _, value := range ticks[:min(len(ticks), 8)] {
		total += value
	}
	idle, iowait := ticks[3], uint64(0)
	if len(ticks) > 4 {
		iowait = ticks[4]
	}
	// idle and iowait are two of the fields total sums, so the subtraction
	// cannot underflow.
	return cores, total - idle - iowait, total, nil
}

// isCPULine reports whether a /proc/stat line's first field names one core:
// "cpu" followed by its number.
func isCPULine(name string) bool {
	rest, ok := strings.CutPrefix(name, "cpu")
	if !ok || rest == "" {
		return false
	}
	for _, digit := range rest {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// parseVMMeminfo reads /proc/meminfo's MemTotal and MemAvailable, which the row
// reports as the memory in use: the total minus what is available. Both are the
// reading itself, so a missing or unreadable one is an error that drops the
// row: mem_bytes cannot be null, so a missing one would read as an empty VM. A
// count that is not in kB, or an available greater than the total, is a damaged
// reading and is refused rather than reported as a negative use.
func parseVMMeminfo(text string) (int64, error) {
	values := make(map[string]uint64, 2)
	for _, raw := range strings.Split(text, "\n") {
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			continue
		}
		key, ok := strings.CutSuffix(fields[0], ":")
		if !ok || (key != "MemTotal" && key != "MemAvailable") {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, fmt.Errorf("meminfo's %s line does not read as a kB count", key)
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("meminfo's %s %s is not a kB count", key, shorten(fields[1]))
		}
		values[key] = value
	}
	total, ok := values["MemTotal"]
	if !ok {
		return 0, errors.New("meminfo carries no MemTotal")
	}
	available, ok := values["MemAvailable"]
	if !ok {
		return 0, errors.New("meminfo carries no MemAvailable")
	}
	if available > total {
		return 0, fmt.Errorf("meminfo's MemAvailable %d kB is more than its MemTotal %d kB", available, total)
	}
	return int64(total-available) * 1024, nil
}
