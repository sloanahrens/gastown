package land

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/daemonlog"
)

// OmStatsDayLayout is the calendar-day form the day tables and --since use.
const OmStatsDayLayout = "2006-01-02"

// OmStatsTopBeads caps the beads-with-most-rejections table: the operator
// reads the worst offenders, not the long tail.
const OmStatsTopBeads = 10

// OmStatsUnnamed labels a rig or agent seat the record leaves blank: a crew
// review records no seat, and a line written before that field carries none
// (gt-mukgk.6).
const OmStatsUnnamed = "(none)"

// OmStats is the town's om verdicts aggregated: how often om rejects a
// landing, by day, rig and author seat, how many review rounds each bead's
// records reached, and the beads rejected most.
type OmStats struct {
	Days     []OmStatsDay   `json:"days"`
	Rigs     []OmStatsGroup `json:"rigs"`
	Agents   []OmStatsGroup `json:"agents"`
	Rounds   OmStatsRounds  `json:"rounds"`
	TopBeads []OmStatsBead  `json:"top_beads"`
	// Skipped counts the review-record lines that were not a record, so they
	// are in none of the counts above.
	Skipped int `json:"skipped"`
}

// OmStatsDay is one calendar day's verdicts. Rate is Rejections over all
// verdicts, 0 on a day without any. MeanFindings and MeanScore average the
// day's rejections and are 0 when it rejected none.
type OmStatsDay struct {
	Day          string  `json:"day"`
	Approvals    int     `json:"approvals"`
	Rejections   int     `json:"rejections"`
	Rate         float64 `json:"rate"`
	MeanFindings float64 `json:"mean_findings"`
	MeanScore    float64 `json:"mean_score"`
	// Fallback marks a day the daemon log filled, before the first review
	// record; such a day counts in the day table alone, because a log line
	// names no rig and no seat.
	Fallback bool `json:"fallback,omitempty"`
}

// OmStatsGroup is one rig's or one agent seat's verdicts over the range.
type OmStatsGroup struct {
	Name       string  `json:"name"`
	Approvals  int     `json:"approvals"`
	Rejections int     `json:"rejections"`
	Rate       float64 `json:"rate"`
}

// OmStatsRounds buckets every bead with a review record by the highest attempt
// that record reached: one om round, two, or three and more. A bead is counted
// whether or not it landed, because a record is written for each verdict, not
// only for a landing.
type OmStatsRounds struct {
	One       int `json:"1"`
	Two       int `json:"2"`
	ThreePlus int `json:"3+"`
}

// OmStatsBead is one bead's rejection count, for the most-rejected table.
type OmStatsBead struct {
	Bead       string `json:"bead"`
	Rig        string `json:"rig,omitempty"`
	Rejections int    `json:"rejections"`
}

// AggregateOmStats reads the review records from records and the landing
// worker's daemon-log lines from log, and aggregates the om verdicts at or
// after since (the zero time for all of them).
//
// The review records are the source. The log fills only the days before the
// first review record — when the reviews file begins — and those days are
// marked Fallback. The log counts in the day table alone: it names no rig, no
// seat and no attempt, so Rounds and TopBeads come from the review records. A
// nil reader reads nothing.
//
// A record line that is not one, or carries no instant, is skipped and counted,
// however long it is; a line the reader cannot read fails the report. Any other
// line of the daemon log is not a verdict and is ignored, not counted.
//
// A review record always names its score and findings, so a zero there is a
// sample. A daemon-log line may omit either, and an absent figure stays out of
// its mean rather than entering as zero.
func AggregateOmStats(records, log io.Reader, since time.Time) (OmStats, error) {
	sinceDay := omStatsDayStart(since)
	stats := OmStats{
		Days:     []OmStatsDay{},
		Rigs:     []OmStatsGroup{},
		Agents:   []OmStatsGroup{},
		TopBeads: []OmStatsBead{},
	}
	days := map[string]*omStatsCounts{}
	rigs := map[string]*omStatsCounts{}
	agents := map[string]*omStatsCounts{}
	beads := map[string]*omStatsBeadAcc{}
	attempts := map[string]int{}
	var firstRecordDay time.Time

	if records != nil {
		recs, skipped, err := readOmStatsRecords(records)
		if err != nil {
			return OmStats{}, err
		}
		stats.Skipped = skipped
		for _, line := range recs {
			rec := line.rec
			at := rec.At.In(time.Local)
			day := at.Format(OmStatsDayLayout)
			if firstRecordDay.IsZero() || at.Before(firstRecordDay) {
				firstRecordDay = omStatsDayStart(at)
			}
			if !sinceDay.IsZero() && at.Before(sinceDay) {
				continue
			}
			rig, agent := orOmStatsUnnamed(rec.Rig), orOmStatsUnnamed(rec.Agent)
			counts := omStatsCountsFor(days, day)
			rigCounts := omStatsCountsFor(rigs, rig)
			agentCounts := omStatsCountsFor(agents, agent)
			if rec.IsRejection() {
				findings, score := float64(rec.Findings), rec.Score
				counts.addRejection(findings, score, line.hasFindings, line.hasScore)
				rigCounts.addRejection(findings, score, line.hasFindings, line.hasScore)
				agentCounts.addRejection(findings, score, line.hasFindings, line.hasScore)
				bead := omStatsBeadFor(beads, rec.BeadID)
				bead.rejections++
				if rec.Rig != "" {
					bead.rig = rec.Rig
				}
			} else {
				counts.approvals++
				rigCounts.approvals++
				agentCounts.approvals++
			}
			if rec.Attempt > attempts[rec.BeadID] {
				attempts[rec.BeadID] = rec.Attempt
			}
		}
	}

	if log != nil {
		events, err := readOmStatsLog(log)
		if err != nil {
			return OmStats{}, err
		}
		landed := map[string]map[string]bool{}
		for _, ev := range events {
			start, err := time.ParseInLocation(OmStatsDayLayout, ev.day, time.Local)
			if err != nil || (!firstRecordDay.IsZero() && !start.Before(firstRecordDay)) {
				continue
			}
			if !sinceDay.IsZero() && start.Before(sinceDay) {
				continue
			}
			counts := omStatsCountsFor(days, ev.day)
			counts.fallback = true
			if ev.rejected {
				counts.addRejection(ev.findings, ev.score, ev.hasFindings, ev.hasScore)
				continue
			}
			// One approval per bead per day: a repair pass logs both "landed"
			// and "already landed as" for the same landing.
			if landed[ev.day] == nil {
				landed[ev.day] = map[string]bool{}
			}
			if !landed[ev.day][ev.bead] {
				landed[ev.day][ev.bead] = true
				counts.approvals++
			}
		}
	}

	for day, counts := range days {
		stats.Days = append(stats.Days, counts.day(day))
	}
	sort.Slice(stats.Days, func(i, j int) bool { return stats.Days[i].Day < stats.Days[j].Day })
	stats.Rigs = omStatsGroups(rigs)
	stats.Agents = omStatsGroups(agents)
	for _, max := range attempts {
		switch {
		case max >= 3:
			stats.Rounds.ThreePlus++
		case max == 2:
			stats.Rounds.Two++
		default:
			stats.Rounds.One++
		}
	}
	stats.TopBeads = omStatsMostRejected(beads)
	return stats, nil
}

// omStatsCounts is one row's running totals. findings and score are sums
// alongside their own sample counts, because a daemon-log rejection can name
// one and not the other.
type omStatsCounts struct {
	approvals, rejections int
	findings              float64
	findingsN             int
	score                 float64
	scoreN                int
	fallback              bool
}

func omStatsCountsFor(m map[string]*omStatsCounts, key string) *omStatsCounts {
	c, ok := m[key]
	if !ok {
		c = &omStatsCounts{}
		m[key] = c
	}
	return c
}

// addRejection counts one rejection; hasFindings and hasScore say whether the
// source named that figure. A daemon-log line can name one and not the other,
// and an absent figure stays out of its mean rather than entering as a zero.
func (c *omStatsCounts) addRejection(findings, score float64, hasFindings, hasScore bool) {
	c.rejections++
	if hasFindings {
		c.findings += findings
		c.findingsN++
	}
	if hasScore {
		c.score += score
		c.scoreN++
	}
}

func (c *omStatsCounts) rate() float64 {
	total := c.approvals + c.rejections
	if total == 0 {
		return 0
	}
	return float64(c.rejections) / float64(total)
}

func (c *omStatsCounts) mean(sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func (c *omStatsCounts) day(key string) OmStatsDay {
	return OmStatsDay{
		Day:          key,
		Approvals:    c.approvals,
		Rejections:   c.rejections,
		Rate:         c.rate(),
		MeanFindings: c.mean(c.findings, c.findingsN),
		MeanScore:    c.mean(c.score, c.scoreN),
		Fallback:     c.fallback,
	}
}

func (c *omStatsCounts) group(name string) OmStatsGroup {
	return OmStatsGroup{
		Name:       name,
		Approvals:  c.approvals,
		Rejections: c.rejections,
		Rate:       c.rate(),
	}
}

// omStatsGroups orders the groups by rejections, then approvals, then name, so
// the seat or rig that rejects most is the first row of a stable table.
func omStatsGroups(m map[string]*omStatsCounts) []OmStatsGroup {
	out := make([]OmStatsGroup, 0, len(m))
	for name, c := range m {
		out = append(out, c.group(name))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rejections != out[j].Rejections {
			return out[i].Rejections > out[j].Rejections
		}
		if out[i].Approvals != out[j].Approvals {
			return out[i].Approvals > out[j].Approvals
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// omStatsBeadAcc is one bead's running rejection count and the rig its last
// record named.
type omStatsBeadAcc struct {
	rig        string
	rejections int
}

func omStatsBeadFor(m map[string]*omStatsBeadAcc, bead string) *omStatsBeadAcc {
	b, ok := m[bead]
	if !ok {
		b = &omStatsBeadAcc{}
		m[bead] = b
	}
	return b
}

// omStatsMostRejected returns the beads with at least one rejection, most
// first, capped at OmStatsTopBeads.
func omStatsMostRejected(m map[string]*omStatsBeadAcc) []OmStatsBead {
	out := make([]OmStatsBead, 0, len(m))
	for bead, b := range m {
		out = append(out, OmStatsBead{Bead: bead, Rig: b.rig, Rejections: b.rejections})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rejections != out[j].Rejections {
			return out[i].Rejections > out[j].Rejections
		}
		return out[i].Bead < out[j].Bead
	})
	if len(out) > OmStatsTopBeads {
		out = out[:OmStatsTopBeads]
	}
	return out
}

// omStatsReviewRecord is one decoded review-record line and whether that line
// named its two mean figures. A ReviewRecord always carries them, but an older
// or hand-written line may not, and an absent figure must stay out of a mean
// instead of entering as zero.
type omStatsReviewRecord struct {
	rec                   ReviewRecord
	hasScore, hasFindings bool
}

// parseOmStatsReviewRecord decodes raw as one ReviewRecord line. The outer
// score and findings fields win over the embedded record's, so the pointers
// report whether the line named each figure.
func parseOmStatsReviewRecord(raw []byte) (omStatsReviewRecord, bool) {
	var line struct {
		ReviewRecord
		Score    *float64 `json:"score"`
		Findings *int     `json:"findings"`
	}
	if err := json.Unmarshal(raw, &line); err != nil || line.At.IsZero() {
		return omStatsReviewRecord{}, false
	}
	out := omStatsReviewRecord{
		rec:         line.ReviewRecord,
		hasScore:    line.Score != nil,
		hasFindings: line.Findings != nil,
	}
	if out.hasScore {
		out.rec.Score = *line.Score
	}
	if out.hasFindings {
		out.rec.Findings = *line.Findings
	}
	return out, true
}

// readOmStatsRecords reads the review-record lines of r. A line that does not
// decode as a ReviewRecord, or carries no instant, is skipped and counted.
func readOmStatsRecords(r io.Reader) ([]omStatsReviewRecord, int, error) {
	var recs []omStatsReviewRecord
	skipped := 0
	err := omStatsScanLines(r, func(raw []byte) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			return
		}
		rec, ok := parseOmStatsReviewRecord(raw)
		if !ok {
			skipped++
			return
		}
		recs = append(recs, rec)
	})
	if err != nil {
		return nil, skipped, fmt.Errorf("reading review records: %w", err)
	}
	return recs, skipped, nil
}

// omStatsScanLines calls fn for each line of r without its line break. It reads
// through a bufio.Reader, so no line has a length limit: one enormous line is
// data for fn to judge, not a read error that kills the whole report.
func omStatsScanLines(r io.Reader, fn func(line []byte)) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			fn(bytes.TrimRight(line, "\r\n"))
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// omStatsLogEvent is one landing the daemon log records: a review rejection
// or a landing that reached the target. hasFindings and hasScore say whether
// the line named that figure, so an unnamed one enters no mean.
type omStatsLogEvent struct {
	day         string
	bead        string
	rejected    bool
	findings    float64
	hasFindings bool
	score       float64
	hasScore    bool
}

var (
	// omStatsLogLineRe splits a landing-worker line into its stamp, bead and
	// the text after it. A line with neither shape is some other daemon work.
	omStatsLogLineRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): (.*)$`)
	// Only a review rejection counts: a gate or infrastructure rejection never
	// reached om. The detail is "om requested changes (score S, N finding(s))".
	omStatsLogRejectRe = regexp.MustCompile(`^rejected \(review\): om requested changes\b`)
	// A landing that reached the target. The earlier "merged" line is not one:
	// the worker merges the candidate before om reviews it, so a rejected
	// landing logs that line too.
	omStatsLogLandedRe = regexp.MustCompile(`^(?:landed [0-9a-f]+ on |already landed as [0-9a-f]+)`)
	omStatsLogScoreRe  = regexp.MustCompile(`\bscore ([0-9.]+)`)
	omStatsLogFindRe   = regexp.MustCompile(`(\d+) finding`)
)

// readOmStatsLog reads the landing-worker lines of r that count as a verdict.
func readOmStatsLog(r io.Reader) ([]omStatsLogEvent, error) {
	var events []omStatsLogEvent
	err := omStatsScanLines(r, func(line []byte) {
		m := omStatsLogLineRe.FindStringSubmatch(string(line))
		if m == nil {
			return
		}
		at, err := time.ParseInLocation(daemonlog.Stamp, m[1], time.Local)
		if err != nil {
			return
		}
		ev := omStatsLogEvent{day: at.Format(OmStatsDayLayout), bead: m[2]}
		switch {
		case omStatsLogRejectRe.MatchString(m[3]):
			ev.rejected = true
			if sm := omStatsLogScoreRe.FindStringSubmatch(m[3]); sm != nil {
				if v, err := strconv.ParseFloat(sm[1], 64); err == nil {
					ev.score, ev.hasScore = v, true
				}
			}
			if fm := omStatsLogFindRe.FindStringSubmatch(m[3]); fm != nil {
				if v, err := strconv.Atoi(fm[1]); err == nil {
					ev.findings, ev.hasFindings = float64(v), true
				}
			}
		case omStatsLogLandedRe.MatchString(m[3]):
		default:
			return
		}
		events = append(events, ev)
	})
	if err != nil {
		return nil, fmt.Errorf("reading the daemon log: %w", err)
	}
	return events, nil
}

// omStatsDayStart is the local midnight of at's calendar day; the zero time
// stays zero, standing for "no bound".
func omStatsDayStart(at time.Time) time.Time {
	if at.IsZero() {
		return time.Time{}
	}
	y, m, d := at.In(time.Local).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

func orOmStatsUnnamed(name string) string {
	if strings.TrimSpace(name) == "" {
		return OmStatsUnnamed
	}
	return name
}
