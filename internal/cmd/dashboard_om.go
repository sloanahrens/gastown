package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/landings"
)

// The om panel reads two things the town already writes: each rig's landings
// file (one record per landed bead, with om's verdict and score) and the
// daemon log (om's wall time per review, and every rejection with its kind).
// It runs no om and no bd.

const (
	omRecentRows   = 15
	omDayRows      = 7
	omStageJoinMin = 45 * time.Minute // a stages line this far before a landing belongs to it
)

var (
	omStageRe  = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): stages: (.*)$`)
	omRejectRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): rejected \((\w+)\): (.*)$`)
	omScoreRe  = regexp.MustCompile(`\bscore ([0-9.]+)`)
)

const omLogTimeLayout = "2006/01/02 15:04:05"

// omStage is one gated merge's stage timings, off a "stages:" log line.
type omStage struct {
	At   time.Time
	Bead string
	OM   *time.Duration
}

// omRejection is one rejected landing, off a "rejected (kind):" log line.
type omRejection struct {
	At     time.Time
	Bead   string
	Kind   string
	Detail string
	Score  *float64
}

// omRecord is a landings record plus the field the landing worker writes for
// a landing that touched risk paths.
type omRecord struct {
	landings.Record
	RiskPaths []string `json:"risk_paths"`
}

// omConfig is the reviewer's own config: what backend, how deep, how strict.
type omConfig struct {
	Backend   []string `json:"backend"`
	Threshold *float64 `json:"threshold"`
	Depth     string   `json:"depth"`
	Timeout   int      `json:"timeout"`
}

// omReader keeps the daemon-log scan between polls, so a poll reads only what
// the log gained, and re-reads the small landings files whole.
type omReader struct {
	landings *dashLandings
	logPath  string
	cfgPath  string

	mu      sync.Mutex
	offset  int64
	stages  []omStage
	rejects []omRejection
}

func newOMReader(townRoot string, landings *dashLandings) *omReader {
	cfg := ""
	if home, err := os.UserHomeDir(); err == nil {
		cfg = filepath.Join(home, ".config", "om", "config.json")
	}
	return &omReader{landings: landings, logPath: filepath.Join(townRoot, "daemon", "daemon.log"), cfgPath: cfg}
}

// read is the panel's whole reading. A reader that fails leaves its part out;
// nothing is ever shown as zero for want of a read.
func (r *omReader) read(now time.Time) *dashboard.OM {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.scanLog(); err != nil && len(r.stages) == 0 && len(r.rejects) == 0 {
		return nil
	}
	recs := r.landings.get()
	return buildOM(now, recs, r.stages, r.rejects, loadOMConfig(r.cfgPath))
}

func readOMRecords(path string) []omRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []omRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var rec omRecord
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Bead != "" {
			out = append(out, rec)
		}
	}
	return out
}

func loadOMConfig(path string) omConfig {
	var c omConfig
	if path == "" {
		return c
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

// scanLog reads what the daemon log gained since the last scan. A log that
// shrank was rotated: it is read from the top, and what was kept stays kept.
func (r *omReader) scanLog() error {
	f, err := os.Open(r.logPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < r.offset {
		r.offset = 0
	}
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			// A last line with no newline yet is not consumed: the writer is
			// mid-line, and the next scan reads it whole.
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		r.offset += int64(len(line))
		if !strings.Contains(line, "[land]") {
			continue
		}
		r.parseLogLine(strings.TrimRight(line, "\r\n"))
	}
}

func (r *omReader) parseLogLine(line string) {
	if m := omStageRe.FindStringSubmatch(line); m != nil {
		if at, err := time.ParseInLocation(omLogTimeLayout, m[1], time.Local); err == nil {
			r.stages = append(r.stages, omStage{At: at, Bead: m[2], OM: omStageDuration(m[3])})
		}
		return
	}
	if m := omRejectRe.FindStringSubmatch(line); m != nil {
		if at, err := time.ParseInLocation(omLogTimeLayout, m[1], time.Local); err == nil {
			rj := omRejection{At: at, Bead: m[2], Kind: m[3], Detail: m[4]}
			if sm := omScoreRe.FindStringSubmatch(m[4]); sm != nil {
				if v, err := strconv.ParseFloat(sm[1], 64); err == nil {
					rj.Score = &v
				}
			}
			r.rejects = append(r.rejects, rj)
		}
	}
}

// omStageDuration reads om's time off "lint 14s, gate 34s, om 40s"; nil when
// the review did not run (no om stage).
func omStageDuration(stages string) *time.Duration {
	for _, part := range strings.Split(stages, ", ") {
		name, rest, ok := strings.Cut(part, " ")
		if !ok || name != "om" {
			continue
		}
		tok, _, _ := strings.Cut(rest, " ")
		if d, err := time.ParseDuration(tok); err == nil {
			return &d
		}
	}
	return nil
}

// omOutcome classifies a landing record's om verdict.
func omOutcome(verdict string) string {
	switch {
	case strings.HasPrefix(verdict, "approve"):
		return "approved"
	case strings.HasPrefix(verdict, "error"):
		return "error"
	}
	// "skipped", and the operator override ("overseer:<sha>"), landed without
	// a review of their own.
	return "skipped"
}

// buildOM folds the records, stages and rejections into the panel's numbers.
func buildOM(now time.Time, recs []omRecord, stages []omStage, rejs []omRejection, cfg omConfig) *dashboard.OM {
	om := &dashboard.OM{Rejects: map[string]int{}, Routes: map[string]int{}}
	if len(cfg.Backend) > 0 {
		om.Backend = filepath.Base(cfg.Backend[0])
	}
	om.Depth, om.TimeoutS, om.Threshold = cfg.Depth, cfg.Timeout, cfg.Threshold

	type win struct {
		label string
		since time.Time
	}
	wins := []win{{"24h", now.Add(-24 * time.Hour)}, {"7d", now.Add(-7 * 24 * time.Hour)}, {"all", time.Time{}}}
	out := make([]dashboard.OMWindow, len(wins))
	scoreSum := make([]float64, len(wins))
	scoreN := make([]int, len(wins))
	secs := make([][]float64, len(wins))
	for i, w := range wins {
		out[i].Label = w.label
	}
	inWin := func(i int, at time.Time) bool { return wins[i].since.IsZero() || !at.Before(wins[i].since) }

	days := map[string]*dashboard.OMDay{}
	daySecs := map[string][]float64{}
	day := func(at time.Time) *dashboard.OMDay {
		k := at.In(time.Local).Format("2006-01-02")
		if days[k] == nil {
			days[k] = &dashboard.OMDay{Day: k}
		}
		return days[k]
	}
	bucket := func(score float64) {
		i := int(score * 10)
		if i < 0 {
			i = 0
		}
		if i > 9 {
			i = 9
		}
		om.Scores[i]++
	}

	var first time.Time
	note := func(at time.Time) {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}

	var recent []dashboard.OMReview
	for _, rec := range recs {
		note(rec.LandedAt)
		oc := omOutcome(rec.OMVerdict)
		om.Routes[rec.Route]++
		if len(rec.RiskPaths) > 0 {
			om.RiskPaths++
		}
		d := day(rec.LandedAt)
		for i := range wins {
			if !inWin(i, rec.LandedAt) {
				continue
			}
			out[i].Landed++
			switch oc {
			case "approved":
				out[i].Approved++
				if rec.OMScore > 0 {
					scoreSum[i] += rec.OMScore
					scoreN[i]++
				}
			case "error":
				out[i].Errors++
			default:
				out[i].Skipped++
			}
		}
		switch oc {
		case "approved":
			d.Approved++
			if rec.OMScore > 0 {
				bucket(rec.OMScore)
			}
		case "error":
			d.Errors++
		default:
			d.Skipped++
		}
		rv := dashboard.OMReview{At: rec.LandedAt, Bead: rec.Bead, Rig: rec.Rig, Outcome: oc, Route: rec.Route,
			Gate: rec.GateResult, Risk: len(rec.RiskPaths) > 0}
		if rec.OMScore > 0 {
			s := rec.OMScore
			rv.Score = &s
		}
		if oc == "error" {
			rv.Detail = truncateRunes(rec.OMVerdict, 120)
		}
		if st := omStageFor(stages, rec.Bead, rec.LandedAt); st != nil && st.OM != nil {
			s := st.OM.Seconds()
			rv.Secs = &s
		}
		recent = append(recent, rv)
	}

	for _, rj := range rejs {
		note(rj.At)
		om.Rejects[rj.Kind]++
		day(rj.At).Rejected++
		for i := range wins {
			if inWin(i, rj.At) {
				out[i].Rejected++
				if rj.Kind == "review" && rj.Score != nil {
					scoreSum[i] += *rj.Score
					scoreN[i]++
				}
			}
		}
		if rj.Kind == "review" && rj.Score != nil {
			bucket(*rj.Score)
		}
		recent = append(recent, dashboard.OMReview{At: rj.At, Bead: rj.Bead, Outcome: "rejected", Kind: rj.Kind,
			Score: rj.Score, Detail: truncateRunes(rj.Detail, 140)})
	}

	for _, st := range stages {
		if st.OM == nil {
			continue
		}
		note(st.At)
		s := st.OM.Seconds()
		for i := range wins {
			if inWin(i, st.At) {
				secs[i] = append(secs[i], s)
			}
		}
		k := st.At.In(time.Local).Format("2006-01-02")
		daySecs[k] = append(daySecs[k], s)
		day(st.At)
	}

	for i := range out {
		if scoreN[i] > 0 {
			v := scoreSum[i] / float64(scoreN[i])
			out[i].AvgScore = &v
		}
		if len(secs[i]) > 0 {
			sort.Float64s(secs[i])
			m, p := percentile(secs[i], 0.5), percentile(secs[i], 0.95)
			out[i].MedianSecs, out[i].P95Secs = &m, &p
		}
	}
	om.Windows = out
	om.Since = first

	keys := make([]string, 0, len(days))
	for k := range days {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	if len(keys) > omDayRows {
		keys = keys[:omDayRows]
	}
	for _, k := range keys {
		d := *days[k]
		if s := daySecs[k]; len(s) > 0 {
			var sum float64
			for _, v := range s {
				sum += v
			}
			avg := sum / float64(len(s))
			d.AvgSecs = &avg
		}
		om.Days = append(om.Days, d)
	}

	sort.SliceStable(recent, func(i, j int) bool { return recent[i].At.After(recent[j].At) })
	if len(recent) > omRecentRows {
		recent = recent[:omRecentRows]
	}
	om.Recent = recent
	return om
}

// omStageFor is the stages line a landing belongs to: the latest one for the
// bead no more than a couple of minutes after the landing and within
// omStageJoinMin before it.
func omStageFor(stages []omStage, bead string, landedAt time.Time) *omStage {
	var best *omStage
	for i := range stages {
		st := &stages[i]
		if st.Bead != bead {
			continue
		}
		if st.At.After(landedAt.Add(2*time.Minute)) || st.At.Before(landedAt.Add(-omStageJoinMin)) {
			continue
		}
		if best == nil || st.At.After(best.At) {
			best = st
		}
	}
	return best
}

// percentile of an ascending slice, nearest rank.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted))+0.5) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
