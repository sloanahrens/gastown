package resourcesampler

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// seatPeakFiles are the cgroup files one seat's end read prints. memory.peak is
// the kernel's high-water mark and cpu.stat the CPU total the peak row carries;
// memory.max names the limit. The read is taken before the gateway deletes the
// seat's pod, so a 30 s sample cannot stand in for it (gt-7l5fbu, design B6).
var seatPeakFiles = []string{"cpu.stat", "memory.peak", "memory.max"}

// PeakRow is one row of bead-peaks.jsonl: the peak of a seat that ended. The
// peaks reader (internal/metrics/peaks.go) takes bead, ended_at, peak_mem_bytes,
// peak_cpu_pct and mem_limit_bytes; the other fields name the seat. A row whose
// bead is null names no landing, so the join passes it over.
type PeakRow struct {
	V             int      `json:"v"`
	Bead          *string  `json:"bead"`
	Rig           *string  `json:"rig"`
	Polecat       *string  `json:"polecat"`
	Sandbox       string   `json:"sandbox"`
	StartedAt     *string  `json:"started_at"`
	EndedAt       string   `json:"ended_at"`
	PeakMemBytes  int64    `json:"peak_mem_bytes"`
	PeakCPUPct    *float64 `json:"peak_cpu_pct"`
	MemLimitBytes *int64   `json:"mem_limit_bytes"`
	Source        string   `json:"source"`
}

// SeatPeakRow builds the bead-peaks row of a seat that has ended, from the text
// of one read of its peak files (the shape seatPeakFiles names). startedAt is
// the sandbox record's created_at and endedAt the time the read was taken, which
// becomes ended_at.
//
// The row is refused, not zeroed, when the seat's name is not a sandbox name,
// its end time is unset, or the read cannot be trusted: memory.peak and cpu.stat
// must each read as a value. A seat with no bead is written with a null bead.
// peak_cpu_pct is the seat's CPU total as a percent of one core over its life;
// it is null when the start is unknown or the life is not positive.
func SeatPeakRow(seat Seat, startedAt, endedAt time.Time, text string) (PeakRow, error) {
	if seat.Name == "" {
		return PeakRow{}, errors.New("sandbox peak: the seat has no name")
	}
	if !validSeatName(seat.Name) {
		return PeakRow{}, fmt.Errorf("sandbox peak: seat name %s is not a sandbox name", shorten(seat.Name))
	}
	if endedAt.IsZero() {
		return PeakRow{}, fmt.Errorf("sandbox peak: seat %s has no end time", seat.Name)
	}
	sections, err := splitSeatPeak(text)
	if err != nil {
		return PeakRow{}, fmt.Errorf("sandbox peak: seat %s: %w", seat.Name, err)
	}
	usage, err := parseUsageUsec(sections["cpu.stat"])
	if err != nil {
		return PeakRow{}, fmt.Errorf("sandbox peak: seat %s: %w", seat.Name, err)
	}
	peak, err := parseCgroupBytes("memory.peak", sections["memory.peak"])
	if err != nil {
		return PeakRow{}, fmt.Errorf("sandbox peak: seat %s: %w", seat.Name, err)
	}
	limit, err := parseMemoryMax(sections["memory.max"])
	if err != nil {
		return PeakRow{}, fmt.Errorf("sandbox peak: seat %s: %w", seat.Name, err)
	}
	var started *string
	if !startedAt.IsZero() {
		formatted := startedAt.UTC().Format(tsLayout)
		started = &formatted
	}
	return PeakRow{
		V:             SchemaVersion,
		Bead:          optionalString(seat.Bead),
		Rig:           optionalString(seat.Rig),
		Polecat:       optionalString(seat.Polecat),
		Sandbox:       seat.Name,
		StartedAt:     started,
		EndedAt:       endedAt.UTC().Format(tsLayout),
		PeakMemBytes:  peak,
		PeakCPUPct:    seatLifetimeCPU(usage, startedAt, endedAt),
		MemLimitBytes: limit,
		Source:        seatSource,
	}, nil
}

// seatLifetimeCPU is the seat's CPU total as a percent of one core over the
// seat's life, rounded to two places as the sampler rounds its percentages. A
// seat with no start, or an end that is not after its start, has no life to
// divide by, so the percent is null rather than a figure invented from it.
func seatLifetimeCPU(usageUsec int64, startedAt, endedAt time.Time) *float64 {
	if startedAt.IsZero() {
		return nil
	}
	life := endedAt.Sub(startedAt).Seconds()
	if life <= 0 {
		return nil
	}
	pct := math.Round(float64(usageUsec)/1e6/life*100*100) / 100
	return &pct
}

// splitSeatPeak splits a seat's peak read into its files' texts. The read has
// the shape seatReadArgv prints: a marker line naming each file in seatPeakFiles,
// the file's text, and the end marker. Each file must appear exactly once, and
// nothing may stand outside a section, so a read that is cut short or carries a
// file it did not ask for is refused rather than read.
func splitSeatPeak(text string) (map[string]string, error) {
	sections := make(map[string]string, len(seatPeakFiles))
	current := ""
	var body []string
	ended := false
	closeSection := func() {
		if current != "" {
			sections[current] = strings.Join(body, "\n")
		}
		current, body = "", nil
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		match := seatCgroupMarker.FindStringSubmatch(line)
		switch {
		case match == nil && ended:
			if strings.TrimSpace(line) != "" {
				return nil, errors.New("the read continues after its end marker")
			}
		case match == nil && current == "":
			if strings.TrimSpace(line) != "" {
				return nil, errors.New("the read starts before any file marker")
			}
		case match == nil:
			body = append(body, line)
		case ended:
			return nil, fmt.Errorf("the read prints %s after its end marker", shorten(match[1]))
		case match[1] == "end":
			closeSection()
			ended = true
		case !slices.Contains(seatPeakFiles, match[1]):
			return nil, fmt.Errorf("the read prints %s, which is not a peak file", shorten(match[1]))
		default:
			closeSection()
			if _, duplicate := sections[match[1]]; duplicate {
				return nil, fmt.Errorf("the read prints %s twice", shorten(match[1]))
			}
			current = match[1]
		}
	}
	closeSection()
	if !ended {
		return nil, errors.New("the read ends before its end marker")
	}
	for _, name := range seatPeakFiles {
		if _, ok := sections[name]; !ok {
			return nil, fmt.Errorf("the read prints no %s", name)
		}
	}
	return sections, nil
}
