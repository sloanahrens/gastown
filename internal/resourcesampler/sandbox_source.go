package resourcesampler

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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

// seatCgroupFiles are the cgroup files one seat read prints, and so the
// sections the parser requires. cpu.stat and memory.current are the reading
// itself; the rest are null when the kernel does not publish them.
var seatCgroupFiles = []string{"cpu.stat", "memory.current", "memory.max", "cpu.pressure", "memory.pressure"}

// seatCgroupMarker is the line a seat read prints before each file's text:
// the file's name between "@@ " and " @@". The last marker is "@@ end @@", so a
// read cut short is refused instead of read as a seat with no pressure.
var seatCgroupMarker = regexp.MustCompile(`^@@ (\S+) @@$`)

// Seat is the identity a live seat's row carries, read from its sandbox record.
type Seat struct {
	// Name is the sandbox name the seat's exec addresses: the record's session.
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
// "@@ end @@" marker.
func SeatRow(seat Seat, text string, now time.Time, previous *SeatReading, workspaces *workspaceid.Map) (Row, SeatReading, error) {
	if seat.Name == "" {
		return Row{}, SeatReading{}, errors.New("sandbox source: the seat has no name")
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

// parseSeatCgroup reads a seat's cgroup text into its readings. The CPU and
// memory readings are required, and a text without them is an error that drops
// the seat's row. The limit and the pressures are null when the kernel does not
// publish them, and an error when the file is there without a readable value,
// so a damaged read is never reported as an absent one.
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
			return nil, fmt.Errorf("the read prints %q after its end marker", name)
		}
		if name == "end" {
			flush()
			ended = true
			continue
		}
		if !slices.Contains(seatCgroupFiles, name) {
			return nil, fmt.Errorf("the read prints %q, which is not one of the seat's cgroup files", name)
		}
		flush()
		if _, duplicate := sections[name]; duplicate {
			return nil, fmt.Errorf("the read prints %s twice", name)
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
// "max" for it, and a kernel that does not publish the file leaves no text.
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
// "full" line. An empty text is a kernel that publishes no pressure, which is
// null; a line that is there without a readable avg10 is an error.
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
			if err != nil || pct < 0 {
				return nil, fmt.Errorf("%s's %s avg10 %s is not a percentage", file, line, shorten(value))
			}
			return &pct, nil
		}
	}
	return nil, fmt.Errorf("%s has no readable %s avg10", file, line)
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
