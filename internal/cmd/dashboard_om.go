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
	"unicode"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/landings"
)

// The om panel reads two things the town already writes: each rig's landings
// file (one record per landed bead, with om's verdict and score) and the
// daemon log (om's wall time per review, and every rejection with its kind).
// It runs no om and no bd.

const (
	omRecentRows = 15
	// The by-day table is a fixed window: today and the three calendar days
	// before it. A day with no reviews still gets a row.
	omDayRows      = 4
	omStageJoinMin = 45 * time.Minute // a stages line this far before a landing belongs to it
	// daemonStartMarker is what the daemon logs when it starts. A landing is
	// one worker's pass, never resumed, so a start after a merge means that
	// landing died with the process: the reader drops its ghost row with it.
	daemonStartMarker = "Daemon starting (PID "
)

var (
	omStageRe  = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): stages: (.*)$`)
	omRejectRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): rejected \((\w+)\): (.*)$`)
	omScoreRe  = regexp.MustCompile(`\bscore ([0-9.]+)`)
	// omMergeRe and omLandedRe bracket a landing: the merge line is logged when
	// the worker starts gating the merged tree, the landed line when it is on
	// the target. A merge with no landed or rejected line after it is in flight.
	omMergeRe  = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): merged `)
	omLandedRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) landing_worker: \[land\] (\S+): (?:landed|already landed as) `)
)

const omLogTimeLayout = "2006/01/02 15:04:05"

// omStage is one gated merge's stage timings, off a "stages:" log line. A
// stage the line does not name is nil; om is nil when the review did not run.
type omStage struct {
	At   time.Time
	Bead string
	Lint *time.Duration
	Gate *time.Duration
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

// omMerge is one landing in flight, off a "merged" log line: the moment the
// worker began gating a bead's merged tree. Done is set by a later landed or
// rejected line for the bead, or by a daemon start, after which the merge is
// no longer live.
type omMerge struct {
	At   time.Time
	Bead string
	Done bool
}

// omRecord is a landings record plus the field the landing worker writes for
// a landing that touched risk paths.
type omRecord struct {
	landings.Record
	RiskPaths []string `json:"risk_paths"`
}

// omConfig is the reviewer's own config: what backend, how deep, how strict.
// Backend is the argv om runs the review with, whose model — not whose
// command — is what the panel and the header strip name.
type omConfig struct {
	Backend   []string `json:"backend"`
	Threshold *float64 `json:"threshold"`
	Depth     string   `json:"depth"`
	Timeout   int      `json:"timeout"`
}

// omModelFamilies are the model names a reviewer's backend is read as, in the
// order a longer name is given up for one of them: a name carrying "sonnet",
// "flash" or "pro" is that family, whatever release prefix its vendor puts on
// it. The short name is what an operator reads off the header, and a release
// number is not what they are looking for when they ask what the town reviews
// with.
var omModelFamilies = []string{"sonnet", "opus", "haiku", "flash", "pro"}

// omModelName names the model a reviewer's backend runs, in the short form an
// operator uses for it. The backend is an argv — ["/<home>/.local/bin/claude",
// "-p", "--model", "sonnet"] is the reviewer's own config — so the model is
// what its --model flag names, and a backend run under a wrapper that names
// none is read from the wrapper's own name ("claude-deepseek-flash" is flash).
// A backend that is neither shows under its command name, which is all the
// town knows about it. What no backend shows is its raw command line.
func omModelName(backend []string) string {
	if len(backend) == 0 {
		return ""
	}
	name := backendModel(backend)
	if name == "" {
		name = filepath.Base(backend[0])
	}
	for _, family := range omModelFamilies {
		if modelWord(name, family) {
			return family
		}
	}
	return name
}

// backendModel returns the model a backend's argv names: the value of --model
// or -m, in either the separate or the --model= spelling. A flag with no value
// after it names nothing.
func backendModel(argv []string) string {
	for i, arg := range argv {
		switch {
		case arg == "--model" || arg == "-m":
			if i+1 < len(argv) {
				return argv[i+1]
			}
		case strings.HasPrefix(arg, "--model="):
			return strings.TrimPrefix(arg, "--model=")
		}
	}
	return ""
}

// modelWord reports whether word is one of name's own words, for a name split
// on anything that is not a letter or a digit: "flash" matches deepseek-flash
// and claude-flash-2026, and never flashback.
func modelWord(name, word string) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if w == word {
			return true
		}
	}
	return false
}

// omReader keeps the daemon-log scan between polls, so a poll reads only what
// the log gained, and re-reads the small landings files whole.
type omReader struct {
	landings *dashLandings
	logPath  string
	cfgPath  string

	mu       sync.Mutex
	offset   int64
	stages   []omStage
	rejects  []omRejection
	merges   map[string]omMerge  // bead -> its latest merge, live until it ends
	lastTick *dashboard.Dispatch // the spec dispatcher's latest tick line
}

func newOMReader(townRoot string, landings *dashLandings) *omReader {
	return &omReader{landings: landings, logPath: filepath.Join(townRoot, "daemon", "daemon.log"), cfgPath: omConfigPath()}
}

// omConfigPath is the reviewer's own config, in the home of whoever runs the
// dashboard: what backend it reviews with, how deep, how strict. Empty when
// there is no home to look in, which every reader of it reads as "no config".
func omConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "om", "config.json")
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

// readOMRecords reads one rig's om record file line by line. A line that is
// not a record is skipped. A reader, not a Scanner: a line longer than the
// scanner's buffer would make the scan stop, silently hiding every record
// after it (gt-2czgm). A read that fails part-way returns what it read.
func readOMRecords(path string) []omRecord {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []omRecord
	br := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			var rec omRecord
			if json.Unmarshal([]byte(strings.TrimRight(line, "\r\n")), &rec) == nil && rec.Bead != "" {
				out = append(out, rec)
			}
		}
		if err != nil {
			return out
		}
	}
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
		if !strings.Contains(line, "[land]") && !strings.Contains(line, dispatchTickMarker) && !strings.Contains(line, daemonStartMarker) {
			continue
		}
		r.parseLogLine(strings.TrimRight(line, "\r\n"))
	}
}

func (r *omReader) parseLogLine(line string) {
	if strings.Contains(line, dispatchTickMarker) {
		if d := parseDispatchTick(line); d != nil {
			r.lastTick = d
		}
		return
	}
	if strings.Contains(line, daemonStartMarker) {
		r.endAllMerges()
		return
	}
	if m := omMergeRe.FindStringSubmatch(line); m != nil {
		if at, err := omLogAt(m[1]); err == nil {
			r.beginMerge(m[2], at)
		}
		return
	}
	if m := omLandedRe.FindStringSubmatch(line); m != nil {
		if at, err := omLogAt(m[1]); err == nil {
			r.endMerge(m[2], at)
		}
		return
	}
	if m := omStageRe.FindStringSubmatch(line); m != nil {
		if at, err := omLogAt(m[1]); err == nil {
			lint, gate, om := omStageTimes(m[3])
			r.stages = append(r.stages, omStage{At: at, Bead: m[2], Lint: lint, Gate: gate, OM: om})
		}
		return
	}
	if m := omRejectRe.FindStringSubmatch(line); m != nil {
		if at, err := omLogAt(m[1]); err == nil {
			rj := omRejection{At: at, Bead: m[2], Kind: m[3], Detail: m[4]}
			if sm := omScoreRe.FindStringSubmatch(m[4]); sm != nil {
				if v, err := strconv.ParseFloat(sm[1], 64); err == nil {
					rj.Score = &v
				}
			}
			r.rejects = append(r.rejects, rj)
			r.endMerge(m[2], at)
		}
	}
}

// omLogAt parses the timestamp a daemon.log line opens with. The log is
// stamped in local time with no zone.
func omLogAt(stamp string) (time.Time, error) {
	return time.ParseInLocation(omLogTimeLayout, stamp, time.Local)
}

// beginMerge records that a bead's tree has been merged and its landing is in
// flight. A landed bead with no later record is re-merged for the repair, and
// a rejected landing can be requeued and merged again: the newest merge is the
// live one, so it replaces whatever the bead had.
func (r *omReader) beginMerge(bead string, at time.Time) {
	if r.merges == nil {
		r.merges = map[string]omMerge{}
	}
	r.merges[bead] = omMerge{At: at, Bead: bead}
}

// endMerge marks a bead's merge no longer in flight. A line at or before the
// merge it would close is ignored: it belongs to an earlier attempt of the
// same bead, and the merge being read is the newer one.
func (r *omReader) endMerge(bead string, at time.Time) {
	m, ok := r.merges[bead]
	if !ok || m.At.After(at) {
		return
	}
	m.Done = true
	r.merges[bead] = m
}

// endAllMerges closes every merge in flight. The worker lands one pass and
// exits; nothing resumes a pass, so a merge a restart followed never finished,
// and one line ends every landing that was live when the daemon came back.
func (r *omReader) endAllMerges() {
	for bead, m := range r.merges {
		m.Done = true
		r.merges[bead] = m
	}
}

// running returns the landings in flight at this read: every merge at or after
// since with no landed or rejected line after it and no daemon start after it,
// newest merge first. Merges the window has passed are dropped, so the map
// stays the size of the window rather than the log.
func (r *omReader) running(since time.Time) []omMerge {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.scanLog()
	var out []omMerge
	for bead, m := range r.merges {
		if m.At.Before(since) {
			delete(r.merges, bead)
			continue
		}
		if !m.Done {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// omStageDuration reads om's time off "lint 14s, gate 34s, om 40s"; nil when
// the review did not run (no om stage).
func omStageDuration(stages string) *time.Duration {
	_, _, om := omStageTimes(stages)
	return om
}

// omStageTimes reads each stage's duration off "lint 14s, gate 34s, om 40s".
// A stage the line does not name comes back nil, as does one whose token is
// not a duration; a trailing "(timed out)" after the token is ignored.
//
// The gate time is the ci stage on a cut-over rig: the candidate gate runs in
// Forgejo CI (StageCI in internal/land), so its line is "ci 2m46s, om 21s". A
// line that names both prefers the explicit gate.
func omStageTimes(stages string) (lint, gate, om *time.Duration) {
	for _, part := range strings.Split(stages, ", ") {
		name, rest, ok := strings.Cut(part, " ")
		if !ok {
			continue
		}
		tok, _, _ := strings.Cut(rest, " ")
		d, err := time.ParseDuration(tok)
		if err != nil {
			continue
		}
		switch name {
		case "lint":
			lint = &d
		case "gate":
			gate = &d
		case "ci":
			if gate == nil {
				gate = &d
			}
		case "om":
			om = &d
		}
	}
	return lint, gate, om
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
	om.Model = omModelName(cfg.Backend)
	om.Depth, om.TimeoutS, om.Threshold = cfg.Depth, cfg.Timeout, cfg.Threshold

	type win struct {
		label string
		since time.Time
	}
	wins := []win{
		{"1h", now.Add(-1 * time.Hour)},
		{"6h", now.Add(-6 * time.Hour)},
		{"24h", now.Add(-24 * time.Hour)},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"all", time.Time{}},
	}
	out := make([]dashboard.OMWindow, len(wins))
	scoreSum := make([]float64, len(wins))
	scoreN := make([]int, len(wins))
	secs := make([][]float64, len(wins))
	for i, w := range wins {
		out[i].Label = w.label
	}
	inWin := func(i int, at time.Time) bool { return wins[i].since.IsZero() || !at.Before(wins[i].since) }

	dayKeys := make([]string, omDayRows)
	dayAllowed := make(map[string]bool, omDayRows)
	today := now.In(time.Local)
	for i := range dayKeys {
		k := today.AddDate(0, 0, -i).Format("2006-01-02")
		dayKeys[i], dayAllowed[k] = k, true
	}

	days := map[string]*dashboard.OMDay{}
	daySecs := map[string][]float64{}
	// day returns the row an event's timestamp belongs to, or nil when its day
	// is older than the three days before today.
	day := func(at time.Time) *dashboard.OMDay {
		k := at.In(time.Local).Format("2006-01-02")
		if !dayAllowed[k] {
			return nil
		}
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
		if d != nil {
			switch oc {
			case "approved":
				d.Approved++
			case "error":
				d.Errors++
			default:
				d.Skipped++
			}
		}
		if oc == "approved" && rec.OMScore > 0 {
			bucket(rec.OMScore)
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
		if d := day(rj.At); d != nil {
			d.Rejected++
		}
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
		if d := day(st.At); d != nil {
			daySecs[d.Day] = append(daySecs[d.Day], s)
		}
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

	for _, k := range dayKeys {
		d := dashboard.OMDay{Day: k}
		if v := days[k]; v != nil {
			d = *v
		}
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

// trendInputs returns the stage lines and rejections the review scan has
// collected at or after since, for the trend panel's window.
func (r *omReader) trendInputs(since time.Time) ([]omStage, []omRejection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.scanLog()
	var stages []omStage
	for _, st := range r.stages {
		if !st.At.Before(since) {
			stages = append(stages, st)
		}
	}
	var rejs []omRejection
	for _, rj := range r.rejects {
		if !rj.At.Before(since) {
			rejs = append(rejs, rj)
		}
	}
	return stages, rejs
}

// dispatch returns the latest dispatcher tick in the log, nil when there is none.
func (r *omReader) dispatch() *dashboard.Dispatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.scanLog(); err != nil && r.lastTick == nil {
		return nil
	}
	if r.lastTick == nil {
		return nil
	}
	d := *r.lastTick
	return &d
}
