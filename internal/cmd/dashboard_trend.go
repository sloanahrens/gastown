package cmd

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat/spans"
)

// The trend panel is the last 24 hours: landings and rejections by local hour,
// the gate and om time of the newest landings, and the host load seen in each
// hour. The landings and stage lines come from the readers the om panel
// already uses; the load history is the one thing the dashboard writes.

const (
	trendHours     = 24
	trendMaxStages = 150
	trendLoadKeep  = 24 * time.Hour
)

// localHour is the local hour t falls in. Landings and stage lines are stamped
// in UTC, so the conversion has to come first: truncating in the writer's own
// zone would bucket those records by their UTC hour.
func localHour(t time.Time) time.Time {
	t = t.In(time.Local)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.Local)
}

// trendWindowStart is the oldest moment the 24 hourly buckets cover.
func trendWindowStart(now time.Time) time.Time {
	return localHour(now).Add(-time.Duration(trendHours-1) * time.Hour)
}

// buildTrend folds the landings, the landing worker's stage lines and
// rejections, and the host's load samples into the last 24 hours. It is pure:
// a record outside the window is not counted, an empty hour is present with
// zeros, and the stage points are thinned to trendMaxStages across the window.
func buildTrend(now time.Time, recs []omRecord, stages []omStage, rejs []omRejection, loads []dashboard.LoadPoint) *dashboard.Trend {
	base := localHour(now)
	hours := make([]dashboard.TrendHour, trendHours)
	idx := make(map[time.Time]int, trendHours)
	for i := range hours {
		hours[i].Hour = base.Add(time.Duration(i-(trendHours-1)) * time.Hour)
		idx[hours[i].Hour] = i
	}
	bucket := func(at time.Time) (int, bool) {
		i, ok := idx[localHour(at)]
		return i, ok
	}

	for _, rec := range recs {
		if i, ok := bucket(rec.LandedAt); ok {
			hours[i].Landed++
		}
	}
	for _, rj := range rejs {
		if i, ok := bucket(rj.At); ok {
			hours[i].Rejected++
		}
	}

	type loadAcc struct {
		sum, max float64
		n        int
	}
	acc := map[int]*loadAcc{}
	for _, p := range loads {
		i, ok := bucket(p.At)
		if !ok {
			continue
		}
		a := acc[i]
		if a == nil {
			a = &loadAcc{max: p.Load}
			acc[i] = a
		}
		a.sum += p.Load
		a.n++
		if p.Load > a.max {
			a.max = p.Load
		}
	}
	for i, a := range acc {
		avg, max := a.sum/float64(a.n), a.max
		hours[i].LoadAvg, hours[i].LoadMax = &avg, &max
	}

	tr := &dashboard.Trend{Hours: hours}
	for _, st := range stages {
		if _, ok := bucket(st.At); !ok {
			continue
		}
		tr.Stages = append(tr.Stages, dashboard.TrendPoint{
			At: st.At, LintSecs: durSecs(st.Lint), GateSecs: durSecs(st.Gate), OMSecs: secsPtr(st.OM),
		})
	}
	sort.SliceStable(tr.Stages, func(i, j int) bool { return tr.Stages[i].At.Before(tr.Stages[j].At) })
	// Too many points for the chart: cut the window into trendMaxStages equal
	// buckets and keep each bucket's slowest point. Keeping the newest points
	// instead would leave a busy day's axis empty before the last few hours.
	if len(tr.Stages) > trendMaxStages {
		start := hours[0].Hour
		span := hours[trendHours-1].Hour.Add(time.Hour).Sub(start)
		slow := func(p dashboard.TrendPoint) float64 {
			s := p.LintSecs + p.GateSecs
			if p.OMSecs != nil {
				s += *p.OMSecs
			}
			return s
		}
		slowest := make(map[int]dashboard.TrendPoint, trendMaxStages)
		for _, p := range tr.Stages {
			b := min(int(int64(p.At.Sub(start))*trendMaxStages/int64(span)), trendMaxStages-1)
			if cur, ok := slowest[b]; !ok || slow(p) > slow(cur) {
				slowest[b] = p
			}
		}
		kept := make([]dashboard.TrendPoint, 0, len(slowest))
		for b := range trendMaxStages {
			if p, ok := slowest[b]; ok {
				kept = append(kept, p)
			}
		}
		tr.Stages = kept
	}
	return tr
}

// recentLandingRows is the most finished landings and rejections the Landings
// table keeps. A busy day lands a few hundred rows and the pane scrolls all of
// them, so the cap never cuts a real day: it is the table's bound against a log
// that runs wild, not a display limit. The live rows, running and failing in
// backoff, sit above the finished ones and are not counted.
const recentLandingRows = 2000

// recentTitleRows is how many of those rows look up a bead title. A title is a
// bd read (cached after the first), so the rows past it show their bead id and
// no title, which keeps a day of landings from costing a read per row (gt-1kttu).
const recentTitleRows = 10

// submissionNameRows bounds the first-sight records one page read keeps: the
// newest few hundred submissions. The log is trimmed by the submission battery
// (internal/jev/submission) to a window of its own, so this bound only bites
// after a burst of submissions, and the rows past it draw a dash rather than
// costing a read each.
const submissionNameRows = 500

// dashSubmitters answers who submitted a bead's landing, for the Landings rows
// whose own record carries no branch: a rejected landing, and one the worker is
// retrying. The name comes from the first-sight submission log's branch
// (gt-l7ciu), and never from the bead, because a rejection reassigns the bead
// or resets it to open, so its current holder is not who submitted it
// (gt-gclus). It does not come from the om review log either: that record's
// agent field is the model the seat ran on ("claude-haiku"), not the polecat or
// crew member the By column says built the landing, so a row with no branch
// draws the dash rather than a model name.
type dashSubmitters struct {
	townRoot string
	// logf reports a read that failed. The operator has no other sight of it:
	// a failed read is not a row, and the page it feeds is the only place the
	// rows appear (gt-gclus).
	logf func(format string, args ...any)

	mu    sync.Mutex
	stamp fileStamp
	read  bool
	names func(rig, bead string) string
	// crew reports whether the bead's newest submission is on a crew session's
	// branch, read from the same records the names are: a crew landing has no
	// polecat seat, and its row says so rather than drawing a dash (gt-kmp7c).
	crew func(rig, bead string) bool
	// failed is the read failure already reported, so a log that stays
	// unreadable reports once rather than on every poll (gt-72xqp).
	failed readFailure
}

// readFailure identifies one failed read: the stamp the log had and the error
// it gave. A poll that sees the same one again has nothing new to report.
type readFailure struct {
	stamp fileStamp
	err   string
}

func newDashSubmitters(townRoot string, logf func(format string, args ...any)) *dashSubmitters {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &dashSubmitters{townRoot: townRoot, logf: logf}
}

// unreadableSubmitters is the lookup while the submission log cannot be read:
// it names "?", the page's mark for a source that could not be read, so a row
// does not claim its submitter is unknown when the answer was merely not
// readable (gt-72xqp).
func unreadableSubmitters(string, string) string { return "?" }

// get returns the name lookup, re-reading the log only when it moved. A log
// that cannot be read names "?" rather than no one: the dash says the
// submitter is unknown, which an unreadable log does not. The failure is
// reported once for the failure it is — the same unreadable log again has
// nothing new to say — and is not remembered as a read: the log is marked read
// only when the read succeeded, so the next poll tries again rather than
// serving the empty answer until the file moves (gt-72xqp).
func (s *dashSubmitters) get() func(rig, bead string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	subPath := land.SubmissionsSeenPath(s.townRoot)
	st, _ := stampFile(subPath)
	if s.read && st == s.stamp {
		return s.names
	}
	subs, err := land.ReadRecentSubmissionRecords(subPath, submissionNameRows)
	if err != nil {
		if f := (readFailure{stamp: st, err: err.Error()}); f != s.failed {
			s.failed = f
			s.logf("the submission log %s could not be read, so the Landings rows it names read \"?\": %v", subPath, err)
		}
		s.crew = noCrew
		return unreadableSubmitters
	}
	s.failed = readFailure{}
	s.names, s.crew, s.stamp, s.read = landingNameLookup(subs), crewLandingLookup(subs), st, true
	return s.names
}

// noCrew answers no for every bead: what a submission log that cannot be read
// says about crew branches, since the dash the row draws claims less than
// calling the landing a crew's would.
func noCrew(string, string) bool { return false }

// crewLookup returns whether a bead's newest submission was built on a crew
// session's branch. It is read from the same log and the same read as the
// names, so a row asks one source for both. A log that cannot be read answers
// no: the row draws a dash, which claims less than calling the landing crew
// would.
func (s *dashSubmitters) crewLookup() func(rig, bead string) bool {
	s.get()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.crew
}

// landingNameLookup builds the lookup the Landings rows ask: the polecat or
// crew member the bead's newest submission names in its branch
// ("polecat/<name>/..." or "crew/<name>/..."), else "" — the page draws that as
// a dash. A resubmission supersedes an earlier branch, so the name is the one
// on the last submitted branch.
func landingNameLookup(subs []land.SubmissionRecord) func(rig, bead string) string {
	type beadKey struct{ rig, bead string }
	branches := make(map[beadKey]string, len(subs))
	for _, s := range subs {
		// The log is oldest first, so the last record of a bead is its newest
		// submission: a resubmission's branch replaces the branch it replaced.
		if s.Branch != "" {
			branches[beadKey{s.Rig, s.Bead}] = s.Branch
		}
	}
	return func(rig, bead string) string {
		return polecatOfBranch(branches[beadKey{rig, bead}])
	}
}

// crewLandingLookup builds the lookup that says whether a bead's newest
// submission is on a crew session's branch ("crew/<agent>/<bead>" or
// "crew/<agent>"): no polecat seat works a crew landing, so its row says crew
// instead of drawing a dash. Like the names, the last submitted branch wins.
func crewLandingLookup(subs []land.SubmissionRecord) func(rig, bead string) bool {
	type beadKey struct{ rig, bead string }
	crew := make(map[beadKey]bool, len(subs))
	for _, s := range subs {
		if s.Branch != "" {
			crew[beadKey{s.Rig, s.Bead}] = isCrewLandingBranch(s.Branch)
		}
	}
	return func(rig, bead string) bool { return crew[beadKey{rig, bead}] }
}

// landingName asks a lookup who submitted a landing. A nil lookup names no one,
// which the page draws as a dash.
func landingName(who func(rig, bead string) string, rig, bead string) string {
	if who == nil {
		return ""
	}
	return who(rig, bead)
}

// buildRecentLandings joins the last 24 hours of landings and rejections with
// the stage lines that say what each cost, newest first, keeping the newest
// limit rows: limit is the table's row bound, and the caller always passes one
// (gt-1j1bf). live is the landings in flight, which go above the bound rather
// than inside it: a landing running now is not one of the day's finished rows.
// The title of a bead is asked for only once the rows to show are known, so the
// lookups are bounded by limit, not by the day's traffic. ship reads a
// landing's ship reading off the deploy tracker, by its rig because what ships
// a landing depends on the rig; a rejection never carries one. The polecat
// time a landing's record carries — the spans-log reading the worker writes
// (gt-yxl22) — comes with it, for the pane's Polecat column (gt-90wi1). who
// names who submitted a row whose record carries no branch: a rejection keeps
// its branch on the bead's rejection note, which a row does not read, so the
// name comes from the first-sight submission log instead (gt-gclus). crew is
// that same log's branch: a rejected or running row built on a crew session's
// branch is marked crew, which the page draws in place of a dash, and a landed
// row is marked from the branch its own record carries. polecat reads a bead's
// polecat time from the spans log: a rejected row takes the reading a landed
// record would carry (asOf zero), and a running row takes the time so far (asOf
// now). Both lookups are asked only for the rows the table keeps — the
// rejections the bound left, and the live rows above it — so the reads are
// bounded like the titles.
func buildRecentLandings(now time.Time, recs []omRecord, stages []omStage, rejs []omRejection, live []dashboard.LandingRow, ship func(rig, bead string) tailShip, title func(rig, bead string) string, who func(rig, bead string) string, crew func(rig, bead string) bool, polecat func(bead string, asOf time.Time) (float64, []land.PolecatAttempt, bool), limit int) []dashboard.LandingRow {
	since := now.Add(-trendHours * time.Hour)
	var rows []dashboard.LandingRow
	rigOf := map[string]string{} // a rejection line names no rig; a landing of the same bead does
	for _, rec := range recs {
		rigOf[rec.Bead] = rec.Rig
	}
	timing := func(r *dashboard.LandingRow, st *omStage) {
		if st == nil {
			return
		}
		r.LintSecs, r.GateSecs, r.OMSecs = secsPtr(st.Lint), secsPtr(st.Gate), secsPtr(st.OM)
	}
	for _, rec := range recs {
		if rec.LandedAt.Before(since) {
			continue
		}
		row := dashboard.LandingRow{
			At: rec.LandedAt, Bead: rec.Bead, Rig: rec.Rig, Polecat: polecatOfBranch(rec.Branch),
			Outcome: "landed", Verdict: omOutcome(rec.OMVerdict), Commit: shortSHA(rec.LandedCommit),
			Route: rec.Route, Risk: len(rec.RiskPaths) > 0, Crew: isCrewLandingBranch(rec.Branch),
		}
		if rec.OMScore > 0 {
			sc := rec.OMScore
			row.Score = &sc
		}
		if row.Verdict == "error" {
			row.Detail = truncateRunes(rec.OMVerdict, 140)
		}
		if ship != nil {
			s := ship(rec.Rig, rec.Bead)
			row.ShipSecs, row.ShipPending = s.Secs, s.Pending
			row.ShipVia, row.ShipFailed, row.ShipRunState = s.Via, s.Failed, s.RunState
		}
		if rec.PolecatSecs > 0 {
			secs := rec.PolecatSecs
			row.PolecatSecs = &secs
		}
		row.PolecatLowerBound, row.PolecatUpperBound = rec.PolecatLowerBound, rec.PolecatUpperBound
		for _, a := range rec.PolecatAttempts {
			row.PolecatAttempts = append(row.PolecatAttempts, dashboard.PolecatAttempt{Seat: a.Seat, Secs: a.Secs, LowerBound: a.LowerBound, UpperBound: a.UpperBound})
		}
		timing(&row, omStageFor(stages, rec.Bead, rec.LandedAt))
		rows = append(rows, row)
	}
	for _, rj := range rejs {
		if rj.At.Before(since) {
			continue
		}
		rig := rigOf[rj.Bead]
		if rig == "" {
			rig = "gastown" // the only rig with a landing worker running
		}
		row := dashboard.LandingRow{At: rj.At, Bead: rj.Bead, Rig: rig, Polecat: landingName(who, rig, rj.Bead), Outcome: "rejected", Kind: rj.Kind, Score: rj.Score, Detail: truncateRunes(rj.Detail, 160)}
		timing(&row, omStageFor(stages, rj.Bead, rj.At))
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.After(rows[j].At) })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if len(live) > 0 {
		rows = append(append([]dashboard.LandingRow(nil), live...), rows...)
	}
	for i := range rows {
		// A landed row carries its own branch; a rejected or running row has
		// none, so a crew build is read from the submission log the name comes
		// from (gt-kmp7c).
		var asOf time.Time
		switch rows[i].Outcome {
		case "rejected":
			asOf = time.Time{} // the reading a landed record would carry
		case "running":
			asOf = now // the time so far
		default:
			continue
		}
		if crew != nil && crew(rows[i].Rig, rows[i].Bead) {
			rows[i].Crew = true
		}
		if polecat != nil {
			secs, attempts, lower := polecat(rows[i].Bead, asOf)
			setPolecatReading(&rows[i], secs, attempts, lower)
		}
	}
	if title != nil {
		seen := map[string]string{}
		for i := range rows {
			if i >= recentTitleRows {
				break
			}
			key := rows[i].Rig + "/" + rows[i].Bead
			t, ok := seen[key]
			if !ok {
				t = title(rows[i].Rig, rows[i].Bead)
				seen[key] = t
			}
			rows[i].Title = t
		}
	}
	return rows
}

// livePolecatName names the builder of a running landing. The bead's holder is
// the polecat when a polecat holds it; a crew member (or a bead nobody holds)
// names no polecat, and the row then names whoever submitted the bead, the same
// name a rejected row draws. Without this fallback the page drew the route
// (the rig's name) in the By cell for a crew-held landing (gt-3hcp9 at 13:52).
func livePolecatName(polecatFor, who func(rig, bead string) string) func(rig, bead string) string {
	return func(rig, bead string) string {
		if polecatFor != nil {
			if p := polecatFor(rig, bead); p != "" {
				return p
			}
		}
		return landingName(who, rig, bead)
	}
}

// runningRows is the landings in flight as Landings rows, newest merge first.
// A live row carries what the merge line holds and nothing the landing has not
// logged yet: no verdict, score or stage times, because the stages line is
// written only once the landing ends. rigFor resolves a bead that has no
// landings record yet to the rig its prefix routes to; polecatFor names who
// holds the bead, read from the bead store once per live row.
func runningRows(live []omMerge, rigFor func(bead string) string, polecatFor func(rig, bead string) string) []dashboard.LandingRow {
	var rows []dashboard.LandingRow
	for _, m := range live {
		rig := ""
		if rigFor != nil {
			rig = rigFor(m.Bead)
		}
		row := dashboard.LandingRow{At: m.At, Bead: m.Bead, Rig: rig, Outcome: "running"}
		if polecatFor != nil {
			row.Polecat = polecatFor(rig, m.Bead)
		}
		rows = append(rows, row)
	}
	return rows
}

// polecatOfAssignee names the polecat a bead is assigned to, off the
// "<rig>/polecats/<name>" assignee a dispatched bead carries. It is empty for
// any other holder: a crew member, or a bead no one has taken.
func polecatOfAssignee(assignee string) string {
	parts := strings.Split(strings.TrimSpace(assignee), "/")
	if len(parts) == 3 && parts[1] == "polecats" {
		return parts[2]
	}
	return ""
}

// landingRig resolves a bead's rig the way a daemon.log line is routed: through
// the town's routes table, read once by the caller and kept. A bead no route
// claims resolves to "", leaving the row's rig blank rather than naming a rig
// that does not hold it.
func landingRig(townRoot string) func(bead string) string {
	rigs := loadTailRigs(townRoot)
	return func(bead string) string {
		if rig := rigs.bead(bead); rig != "town" {
			return rig
		}
		return ""
	}
}

// polecatOfBranch names who built a landing from its branch: the polecat in a
// "polecat/<name>/<bead>+<nonce>" branch, the crew member in a
// "crew/<name>/<bead>" or "crew/<name>" branch, or the first path element of
// any other branch ("sloan/dashboard" is sloan's, an operator landing). It is
// empty when the branch says nothing: a crew branch that names no one is empty
// too, never the operator rule's "crew".
func polecatOfBranch(branch string) string {
	parts := strings.Split(branch, "/")
	switch {
	case len(parts) >= 3 && parts[0] == "polecat":
		return parts[1]
	case parts[0] == "crew":
		if len(parts) >= 2 {
			return parts[1]
		}
		return ""
	case len(parts) >= 2 && parts[0] != "polecat" && parts[0] != "":
		return parts[0]
	}
	return ""
}

// isCrewLandingBranch reports whether a landing's branch is a crew session's
// build ("crew/<agent>/<bead>"), which has no polecat seat to read time from.
func isCrewLandingBranch(branch string) bool {
	return strings.HasPrefix(branch, "crew/")
}

// setPolecatReading draws a row's polecat time from the spans log: a reading
// with no time reads as a dash, and each seat's share is listed as a landed
// record's is. A row keeps the lower-bound mark the reading carries.
func setPolecatReading(row *dashboard.LandingRow, secs float64, attempts []land.PolecatAttempt, lowerBound bool) {
	row.PolecatSecs, row.PolecatAttempts, row.PolecatLowerBound = nil, nil, lowerBound
	if secs > 0 {
		s := secs
		row.PolecatSecs = &s
	}
	for _, a := range attempts {
		row.PolecatAttempts = append(row.PolecatAttempts, dashboard.PolecatAttempt{Seat: a.Seat, Secs: a.Secs, LowerBound: a.LowerBound, UpperBound: a.UpperBound})
	}
}

// spanPolecatTimes is the Landings pane's polecat-time reader: the landing
// worker's own reader over the town's spans log, so a rejected or running row
// and a landed record read the one way. The store under it keeps each bead's
// summary until its log moves, so a poll reads a rejected bead's log once
// rather than on every publish (gt-kmp7c).
func spanPolecatTimes(townRoot string) func(bead string, asOf time.Time) (float64, []land.PolecatAttempt, bool) {
	f := &land.LandingsFile{Spans: newSpanCache(spans.DefaultDir(townRoot))}
	return f.PolecatTime
}

// spanCacheRows bounds what the span cache keeps: the bead summaries of a
// day's landings, well past the rows the pane shows. Past it the cache starts
// over, so a dashboard that runs for weeks spends a re-read on the beads it
// still shows rather than holding every bead it ever read.
const spanCacheRows = 512

// spanCache is the dashboard's span store: one bead's summary, kept until the
// bead's log changes. A rejected bead's log never moves again, so re-reading it
// on every poll would spend the disk on an answer that cannot have changed,
// while a running bead's log is stamped anew by each open and close, so its
// time so far is read again (gt-kmp7c).
type spanCache struct {
	dir   string
	store *spans.Store

	mu     sync.Mutex
	byBead map[string]cachedSpan
}

type cachedSpan struct {
	stamp fileStamp
	sum   spans.Summary
}

func newSpanCache(dir string) *spanCache {
	return &spanCache{dir: dir, store: spans.New(dir, nil)}
}

// Summarize reads a bead's spans log, answering from the last read while the
// log's stamp is unchanged. Only a read that succeeded is kept: a log that
// could not be read is asked again on the next poll rather than pinning the
// failure until something writes the file.
func (c *spanCache) Summarize(bead string) (spans.Summary, error) {
	// The bead names the log, so it is checked before it can reach a path; the
	// store checks it again and refuses an id that is not one path component.
	if !spans.ValidBeadID(bead) {
		return c.store.Summarize(bead)
	}
	st, err := stampFile(filepath.Join(c.dir, bead+".jsonl"))
	if err != nil {
		// A bead with no log recorded nothing: the store's empty answer. There
		// is nothing to keep either, since a log that appears gets a stamp.
		return c.store.Summarize(bead)
	}
	c.mu.Lock()
	old, ok := c.byBead[bead]
	c.mu.Unlock()
	if ok && old.stamp == st {
		return old.sum, nil
	}
	sum, err := c.store.Summarize(bead)
	if err != nil {
		return sum, err
	}
	c.mu.Lock()
	if c.byBead == nil || len(c.byBead) >= spanCacheRows {
		c.byBead = make(map[string]cachedSpan, spanCacheRows)
	}
	c.byBead[bead] = cachedSpan{stamp: st, sum: sum}
	c.mu.Unlock()
	return sum, nil
}

// EndOpen is the writer's half of the store the landing worker reads times
// through. The dashboard only reads, so it hands the store's own answer on
// rather than keeping a record the worker would write.
func (c *spanCache) EndOpen(bead, seat string, reason spans.Reason, at time.Time) (bool, error) {
	return c.store.EndOpen(bead, seat, reason, at)
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func durSecs(d *time.Duration) float64 {
	if d == nil {
		return 0
	}
	return d.Seconds()
}

func secsPtr(d *time.Duration) *float64 {
	if d == nil {
		return nil
	}
	v := d.Seconds()
	return &v
}

// loadSample is one line of the load history file.
type loadSample struct {
	At   time.Time `json:"at"`
	Load float64   `json:"load"`
}

// dashLoads is the dashboard's record of the host's load: one sample per
// machine poll, appended to <town>/.runtime/dashboard-load.jsonl so a restart
// keeps the 24-hour trend. That file is the only thing the dashboard writes. A
// missing or unreadable file is no history, never an error the page shows.
type dashLoads struct {
	path string
	now  func() time.Time

	mu  sync.Mutex
	pts []dashboard.LoadPoint
	// sinceRewrite counts the appends since the file was last rewritten
	// wholesale, so a long-lived run trims it periodically instead of
	// appending forever (gt-2czgm).
	sinceRewrite int
}

// loadRewriteEvery is how many appends the writer lets accumulate before it
// rewrites the file with just the kept window. The hub samples about every
// 10s while a page is open, so this is roughly an hour's drift past a
// 24-hour window before the file is trimmed back.
const loadRewriteEvery = 360

func newDashLoads(townRoot string, now func() time.Time) *dashLoads {
	l := &dashLoads{path: filepath.Join(constants.TownRuntimePath(townRoot), "dashboard-load.jsonl"), now: now}
	read := l.read()
	l.pts = l.trim(read)
	if len(read) > 0 {
		l.rewrite(l.pts)
	}
	return l
}

// append records one machine sample in memory and appends it to the file. A
// write that fails leaves the in-memory history serving the page.
//
// Every loadRewriteEvery appends it rewrites the file with just the kept
// window, so the file does not grow with every sample a long-lived dashboard
// ever took. The hub samples about every 10s while a page is open, so the file
// drifts at most an hour past the window before it is trimmed (gt-2czgm).
func (l *dashLoads) append(at time.Time, load float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pts = l.trim(append(l.pts, dashboard.LoadPoint{At: at, Load: load}))
	if len(l.pts) == 0 {
		return
	}
	l.sinceRewrite++
	if l.sinceRewrite >= loadRewriteEvery {
		l.sinceRewrite = 0
		l.rewrite(l.pts)
		return
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(loadLine(nil, l.pts[len(l.pts)-1]))
}

// points returns the load history, oldest first.
func (l *dashLoads) points() []dashboard.LoadPoint {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]dashboard.LoadPoint(nil), l.pts...)
}

// read parses the file's JSON lines. A line that is not a sample is skipped.
func (l *dashLoads) read() []dashboard.LoadPoint {
	f, err := os.Open(l.path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []dashboard.LoadPoint
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var s loadSample
		if json.Unmarshal(sc.Bytes(), &s) == nil && !s.At.IsZero() {
			out = append(out, dashboard.LoadPoint{At: s.At, Load: s.Load})
		}
	}
	return out
}

// trim drops the samples older than the trend window.
func (l *dashLoads) trim(pts []dashboard.LoadPoint) []dashboard.LoadPoint {
	cutoff := l.now().Add(-trendLoadKeep)
	var kept []dashboard.LoadPoint
	for _, p := range pts {
		if !p.At.Before(cutoff) {
			kept = append(kept, p)
		}
	}
	return kept
}

// rewrite replaces the file with the kept samples, trimming what a previous
// run left behind. The write goes to a temp file renamed over the target, so a
// concurrent reader — a second dashboard, or a restart racing this write —
// never sees a partially written history (gt-2czgm).
func (l *dashLoads) rewrite(pts []dashboard.LoadPoint) {
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	var b []byte
	for _, p := range pts {
		b = loadLine(b, p)
	}
	tmp, err := os.CreateTemp(dir, "dashboard-load-*.jsonl")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return
	}
	// CreateTemp makes the file 0600; the history was 0644 and other readers
	// (an operator, a second dashboard run by the same user) expect it.
	if err := os.Chmod(name, 0o644); err != nil {
		_ = os.Remove(name)
		return
	}
	if err := os.Rename(name, l.path); err != nil {
		_ = os.Remove(name)
	}
}

func loadLine(b []byte, p dashboard.LoadPoint) []byte {
	line, err := json.Marshal(loadSample{At: p.At, Load: p.Load})
	if err != nil {
		return b
	}
	return append(append(b, line...), '\n')
}
