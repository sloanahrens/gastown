package resourcesampler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// SandboxCluster is the cluster the sandbox rows name, as the sampling design's
// row table gives it (docs/plans/2026-11-14-sandbox-resource-sampling-design.md).
const SandboxCluster = "gt-sbx"

// The seat rows' schema values (design row table).
const (
	seatSource    = "sandbox"
	seatClass     = "sandbox"
	seatComponent = "seat"
)

// seatCgroupFiles are the cgroup files one seat read prints. The parser
// requires a section for every one of them: a file the kernel does not publish
// is printed as a marker with no text after it, never as a missing section.
// cpu.stat and memory.current are the reading itself; the rest are null when
// their section carries no text.
var seatCgroupFiles = []string{"cpu.stat", "memory.current", "memory.max", "cpu.pressure", "memory.pressure"}

// seatCgroupMarker is the line a seat read prints before each file's text:
// the file's name between "@@ " and " @@". The last marker is "@@ end @@", so a
// read cut short is refused instead of read as a seat with no pressure.
var seatCgroupMarker = regexp.MustCompile(`^@@ (\S+) @@$`)

// Seat is the identity a live seat's row carries, read from its sandbox record.
type Seat struct {
	// Name is the sandbox name the seat's exec addresses: the record's session.
	// SeatRow refuses one that is not a sandbox name, since the name becomes an
	// exec argument.
	Name    string
	Rig     string
	Polecat string
	// Bead is the bead the seat was spawned for: the record's WorkBead.
	Bead string
}

// SeatReading is a seat's cgroup CPU reading at one sample: the baseline its
// next row's cpu_pct and interval_s are measured from.
type SeatReading struct {
	UsageUsec int64
	At        time.Time
}

// seatCgroup is one seat's parsed cgroup readings; a nil reading is one the
// kernel did not publish.
type seatCgroup struct {
	usageUsec int64
	memBytes  int64
	memLimit  *int64
	cpuSome   *float64
	memSome   *float64
	memFull   *float64
}

// SeatRow builds the one schema v1 row a live seat's cgroup text describes, and
// returns the reading to pass as previous on the seat's next sample. previous
// is nil for the seat's first sample. An error, not a row, is what a text that
// does not read as a live seat produces: a seat that cannot be read writes no
// row rather than one of zeros (gt-kuncsk).
//
// The text is what one read of the seat's cgroup files prints: for each file in
// seatCgroupFiles, a marker line naming it and then its contents, and a closing
// "@@ end @@" marker. A file the kernel does not publish is printed as its
// marker with no text after it, so every file keeps its section.
func SeatRow(seat Seat, text string, now time.Time, previous *SeatReading, workspaces *workspaceid.Map) (Row, SeatReading, error) {
	if seat.Name == "" {
		return Row{}, SeatReading{}, errors.New("sandbox source: the seat has no name")
	}
	if !validSeatName(seat.Name) {
		return Row{}, SeatReading{}, fmt.Errorf("sandbox source: seat name %s is not a sandbox name", shorten(seat.Name))
	}
	if workspaces == nil {
		return Row{}, SeatReading{}, errors.New("sandbox source: no workspace map")
	}
	cgroup, err := parseSeatCgroup(text)
	if err != nil {
		return Row{}, SeatReading{}, fmt.Errorf("sandbox source: seat %s: %w", seat.Name, err)
	}

	reading := SeatReading{UsageUsec: cgroup.usageUsec, At: now}
	interval, cpuPct := seatCPU(reading, previous)
	cluster := SandboxCluster
	row := Row{
		V:                  SchemaVersion,
		TS:                 now.UTC().Format(tsLayout),
		IntervalS:          interval,
		Source:             seatSource,
		Class:              seatClass,
		Component:          seatComponent,
		Name:               seat.Name,
		Rig:                optionalString(seat.Rig),
		Polecat:            optionalString(seat.Polecat),
		Bead:               optionalString(seat.Bead),
		CPUPct:             cpuPct,
		MemBytes:           cgroup.memBytes,
		MemLimitBytes:      cgroup.memLimit,
		Cluster:            &cluster,
		PressureCPUSomePct: cgroup.cpuSome,
		PressureMemSomePct: cgroup.memSome,
		PressureMemFullPct: cgroup.memFull,
	}
	row.SetWorkspace(workspaces)
	return row, reading, nil
}

// seatCPU returns the interval_s and cpu_pct of reading measured against the
// seat's previous one. The first sample has no previous row to measure from, so
// it reports interval_s 0 with null CPU. A usage_usec that went backwards is a
// new cgroup, so it reports the interval with null CPU; a gap that interval_s
// cannot carry, including a clock that did not advance, reports interval_s 0
// and null CPU, since dividing by the zero interval it rounded to would invent
// a percentage.
func seatCPU(reading SeatReading, previous *SeatReading) (float64, *float64) {
	if previous == nil {
		return 0, nil
	}
	interval := time.Duration(math.Round(reading.At.Sub(previous.At).Seconds()*10)) * time.Second / 10
	if interval <= 0 {
		return 0, nil
	}
	if reading.UsageUsec < previous.UsageUsec {
		return interval.Seconds(), nil
	}
	seconds := float64(reading.UsageUsec-previous.UsageUsec) / 1e6
	pct := math.Round(seconds/interval.Seconds()*100*100) / 100
	return interval.Seconds(), &pct
}

// DefaultSeatTimeout bounds one seat's read. A cgroup read prints a handful of
// small files, so a seat that takes longer is a wedged gateway call, and the
// bound leaves the patrol's own minute (B3) room for a cluster of them
// (gt-a24bcc).
const DefaultSeatTimeout = 2 * time.Second

// seatCgroupRoot is where a sandbox's own cgroup is mounted: the v2 unified
// hierarchy, whose files seatCgroupFiles names.
const seatCgroupRoot = "/sys/fs/cgroup"

// SeatExec runs one command inside a sandbox, the method set of *sandbox.CLI's
// Exec, which the daemon fills this seam with (B3).
type SeatExec interface {
	Exec(ctx context.Context, name string, env map[string]string, argv []string, stdin []byte) (stdout, stderr []byte, err error)
}

// SeatSkip is a live seat the tick wrote no row for, and why: its record could
// not be read, its name is not one an exec may address, or its read failed.
type SeatSkip struct {
	Seat string
	Err  error
}

// SeatTick is one tick's outcome: the rows of the seats that read and one skip
// per seat that did not, each in seat name order.
type SeatTick struct {
	Rows    []Row
	Skipped []SeatSkip
}

// SeatSource reads each live seat's cgroup files into one row per seat, the
// seats being the sandbox records under the town (gt-a24bcc). It keeps each
// seat's last good reading between ticks, so cpu_pct is the delta over the
// seat's own previous sample (gt-bru6l7). Tick is not safe for concurrent use:
// the patrol ticks serially, and the readings are not locked.
type SeatSource struct {
	townRoot string
	runner   SeatExec
	clock    Clock
	timeout  time.Duration
	// previous is each seat's last reading that wrote a row, keyed by the
	// sandbox record's name. A seat with no entry is sampled for the first time.
	previous map[string]SeatReading
}

// NewSeatSource returns the source a patrol ticks: each seat read through
// runner, at most timeout after it starts, sampled at clock's time.
func NewSeatSource(townRoot string, runner SeatExec, clock Clock, timeout time.Duration) *SeatSource {
	if clock == nil {
		clock = SystemClock{}
	}
	if timeout <= 0 {
		timeout = DefaultSeatTimeout
	}
	return &SeatSource{townRoot: townRoot, runner: runner, clock: clock, timeout: timeout}
}

// Tick reads every live seat once and returns the rows of those that read. Each
// row's cpu_pct is measured against the seat's previous reading that wrote a
// row, so the first tick a seat is read reports null CPU.
//
// A failure of the whole source — no town root, no exec seam, a workspace map or
// a registry directory that cannot be read — is an error and no rows. One seat's
// failure is not the tick's: it is reported in Skipped and the other seats still
// write, since one wedged sandbox says nothing about the rest. A seat that failed
// keeps its last reading, so its next row is measured over the whole gap.
func (s *SeatSource) Tick(ctx context.Context) (SeatTick, error) {
	if s.runner == nil {
		return SeatTick{}, errors.New("sandbox source: no exec seam")
	}
	// The map is read before any seat, so a bad map costs no exec and no row:
	// a row stamped with an id the operator did not ask for cannot be told from
	// a real one (gt-3mrzx). It is also what refuses an empty town root.
	workspaces, err := workspaceid.Load(s.townRoot)
	if err != nil {
		return SeatTick{}, fmt.Errorf("sandbox source: %w", err)
	}
	records, err := seatRecords(s.townRoot)
	if err != nil {
		return SeatTick{}, fmt.Errorf("sandbox source: %w", err)
	}
	now := s.clock.Now()
	argv := seatReadArgv()
	if s.previous == nil {
		s.previous = make(map[string]SeatReading)
	}
	listed := make(map[string]bool, len(records))
	var tick SeatTick
	for _, record := range records {
		listed[record.name] = true
		if record.err != nil {
			tick.skip(record.name, record.err)
			continue
		}
		if !record.found {
			continue
		}
		seat, err := seatFromRecord(record.rec)
		if err != nil {
			tick.skip(record.name, err)
			continue
		}
		text, err := s.readSeat(ctx, seat.Name, argv)
		if err != nil {
			tick.skip(seat.Name, err)
			continue
		}
		var previous *SeatReading
		if last, ok := s.previous[record.name]; ok {
			previous = &last
		}
		row, reading, err := SeatRow(seat, text, now, previous, workspaces)
		if err != nil {
			tick.skip(seat.Name, err)
			continue
		}
		s.previous[record.name] = reading
		tick.Rows = append(tick.Rows, row)
	}
	// A seat whose record is gone has ended. Dropping its reading means a seat
	// that comes back under the same name starts from a first sample, not from
	// the old cgroup's counter.
	for name := range s.previous {
		if !listed[name] {
			delete(s.previous, name)
		}
	}
	return tick, nil
}

// skip records a seat the tick wrote no row for.
func (t *SeatTick) skip(seat string, err error) {
	t.Skipped = append(t.Skipped, SeatSkip{Seat: seat, Err: err})
}

// readSeat runs one seat's cgroup read under the source's per-seat timeout, so
// a wedged gateway call costs its own seat and not the tick. The name has
// already been validated by seatFromRecord: it reaches the seam as an argument
// of the exec and nowhere else.
func (s *SeatSource) readSeat(ctx context.Context, name string, argv []string) (string, error) {
	seatCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	stdout, _, err := s.runner.Exec(seatCtx, name, nil, argv, nil)
	if err != nil {
		return "", fmt.Errorf("seat %s: %w", name, err)
	}
	return string(stdout), nil
}

// seatReadArgv is the argv one seat read runs: a shell that prints each of the
// seat's cgroup files under the marker line SeatRow's parser reads it by, then
// the end marker. A file the kernel does not publish prints its marker and
// nothing else, which parses as null where the file is optional and as a
// refused read where it is the reading itself; the shell runs every file even
// when an earlier one is missing, and the print after each file keeps a text
// without a trailing newline off the next marker.
func seatReadArgv() []string {
	var script strings.Builder
	for _, name := range seatCgroupFiles {
		fmt.Fprintf(&script, "printf '@@ %s @@\\n'\ncat %s/%s 2>/dev/null\nprintf '\\n'\n", name, seatCgroupRoot, name)
	}
	script.WriteString("printf '@@ end @@\\n'\n")
	return []string{"sh", "-c", script.String()}
}

// seatRecord is one file under the sandbox registry: the session its name
// gives, the record it holds (found), and the error that kept it from being
// read.
type seatRecord struct {
	name  string
	rec   session.SandboxRecord
	found bool
	err   error
}

// seatRecords lists the town's sandbox records in file name order. A registry
// directory that is missing is a town that never ran a sandbox: no records and
// no error. One that cannot be listed is an error, since a tick that cannot
// look must not report the same empty answer as a cluster with no seat.
func seatRecords(townRoot string) ([]seatRecord, error) {
	dir := session.SandboxRegistryDir(townRoot)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing the sandbox records in %s: %w", dir, err)
	}
	var records []seatRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		rec, found, err := session.ReadSandboxRecord(townRoot, name)
		// !found with no error: another process removed the record between the
		// listing and the read, so the seat has ended and writes no row.
		records = append(records, seatRecord{name: name, rec: rec, found: found, err: err})
	}
	return records, nil
}

// seatFromRecord is the identity a record's row carries. It refuses a name the
// exec must not address, which is the check that keeps a bad name out of the
// seam: SeatRow's own name check runs after the seat has been read.
func seatFromRecord(rec session.SandboxRecord) (Seat, error) {
	if rec.Session == "" {
		return Seat{}, errors.New("the record names no seat")
	}
	if !validSeatName(rec.Session) {
		return Seat{}, fmt.Errorf("seat name %s is not a sandbox name", shorten(rec.Session))
	}
	return Seat{Name: rec.Session, Rig: rec.Rig, Polecat: rec.Polecat, Bead: rec.WorkBead}, nil
}

// parseSeatCgroup reads a seat's cgroup text into its readings. cpu.stat and
// memory.current carry the reading itself, so a section without a value there
// is an error that drops the seat's row. memory.max and the pressure files are
// null when their section carries no text, which is how the read prints a file
// the kernel does not publish; a section that carries text without a readable
// value is an error, so a damaged reading is never reported as an absent one.
func parseSeatCgroup(text string) (seatCgroup, error) {
	sections, err := splitSeatCgroup(text)
	if err != nil {
		return seatCgroup{}, err
	}
	var cgroup seatCgroup
	if cgroup.usageUsec, err = parseUsageUsec(sections["cpu.stat"]); err != nil {
		return seatCgroup{}, err
	}
	if cgroup.memBytes, err = parseCgroupBytes("memory.current", sections["memory.current"]); err != nil {
		return seatCgroup{}, err
	}
	if cgroup.memLimit, err = parseMemoryMax(sections["memory.max"]); err != nil {
		return seatCgroup{}, err
	}
	if cgroup.cpuSome, err = parsePressure("cpu.pressure", "some", sections["cpu.pressure"]); err != nil {
		return seatCgroup{}, err
	}
	if cgroup.memSome, err = parsePressure("memory.pressure", "some", sections["memory.pressure"]); err != nil {
		return seatCgroup{}, err
	}
	if cgroup.memFull, err = parsePressure("memory.pressure", "full", sections["memory.pressure"]); err != nil {
		return seatCgroup{}, err
	}
	return cgroup, nil
}

// splitSeatCgroup splits a seat read into its files' texts. Every file in
// seatCgroupFiles must appear once, the end marker must close the text, and
// nothing may stand outside a section: a read that does not have that shape is
// not a seat's cgroup and is refused.
func splitSeatCgroup(text string) (map[string]string, error) {
	sections := make(map[string]string, len(seatCgroupFiles))
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
		if !slices.Contains(seatCgroupFiles, name) {
			return nil, fmt.Errorf("the read prints %s, which is not one of the seat's cgroup files", shorten(name))
		}
		flush()
		if _, duplicate := sections[name]; duplicate {
			return nil, fmt.Errorf("the read prints %s twice", shorten(name))
		}
		current, body = name, nil
	}
	// The last section is closed here only so a read that stops before its end
	// marker is reported as truncated, not as one missing a file it printed.
	flush()
	if !ended {
		return nil, errors.New("the read ends before its end marker")
	}
	for _, name := range seatCgroupFiles {
		if _, ok := sections[name]; !ok {
			return nil, fmt.Errorf("the read prints no %s", name)
		}
	}
	return sections, nil
}

// parseUsageUsec reads cpu.stat's usage_usec, the counter cpu_pct is a delta of.
func parseUsageUsec(text string) (int64, error) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "usage_usec" {
			continue
		}
		usec, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || usec < 0 {
			return 0, fmt.Errorf("cpu.stat's usage_usec %s is not a count of microseconds", shorten(fields[1]))
		}
		return usec, nil
	}
	return 0, errors.New("cpu.stat carries no usage_usec")
}

// parseCgroupBytes reads a single-number cgroup file's text.
func parseCgroupBytes(name, text string) (int64, error) {
	value := strings.TrimSpace(text)
	bytes, err := strconv.ParseInt(value, 10, 64)
	if err != nil || bytes < 0 {
		return 0, fmt.Errorf("%s %s is not a byte count", name, shorten(value))
	}
	return bytes, nil
}

// parseMemoryMax reads memory.max, which is null for no limit: the file prints
// "max" for it, and a file the kernel does not publish reads as an empty text.
func parseMemoryMax(text string) (*int64, error) {
	value := strings.TrimSpace(text)
	if value == "" || value == "max" {
		return nil, nil
	}
	limit, err := parseCgroupBytes("memory.max", value)
	if err != nil {
		return nil, err
	}
	return &limit, nil
}

// parsePressure reads one line's avg10 from a PSI file's text: the "some" or
// "full" line. An empty text is a file the kernel does not publish, which is
// null; a line that is there without a readable avg10 is an error. The reading
// must be a finite number, since a NaN or an infinity cannot be written to the
// row's JSON.
func parsePressure(file, line, text string) (*float64, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	for _, raw := range strings.Split(text, "\n") {
		fields := strings.Fields(raw)
		if len(fields) == 0 || fields[0] != line {
			continue
		}
		for _, field := range fields[1:] {
			value, ok := strings.CutPrefix(field, "avg10=")
			if !ok {
				continue
			}
			pct, err := strconv.ParseFloat(value, 64)
			if err != nil || pct < 0 || math.IsNaN(pct) || math.IsInf(pct, 0) {
				return nil, fmt.Errorf("%s's %s avg10 %s is not a percentage", file, line, shorten(value))
			}
			return &pct, nil
		}
	}
	return nil, fmt.Errorf("%s has no readable %s avg10", file, line)
}

// validSeatName reports whether name has the shape a kubernetes object name
// has: a DNS subdomain, which is the rule the control node read applies to the
// name it puts in a request path (internal/daemon/attention_host_node.go). The
// seat name becomes an exec argument, so one outside that shape is refused
// rather than passed on. The rule is restated here because package daemon
// imports this one, and cannot be imported back.
func validSeatName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// optionalString is the row's pointer field for an identity the record may not
// name: an absent name is null, never an empty string.
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// shorten quotes text for an error, cut to what a log line carries: the text
// came from a seat, and an error must not copy a whole file into the log.
func shorten(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 40 {
		text = text[:40] + "..."
	}
	return strconv.Quote(text)
}
