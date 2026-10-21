package dashboard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// The Claude plan's month rollup folds the weekly windows into the plan's own
// billing month, which runs from one renewal day to the next, so the operator
// can see whether a $200 plan is the right size (gt-bcq4m). Closed windows are
// recorded in weeks.jsonl beside the samples, so the rollup outlives the series.

const (
	// ClaudePlanDefaultRenewalDay is the renewal day when the operator names none.
	ClaudePlanDefaultRenewalDay = 18

	// ClaudePlanCeilingPct is the weekly percentage that counts as a week at the
	// ceiling; the page mirrors it for the line's color.
	ClaudePlanCeilingPct = 90.0

	// claudePlanWeeksFile is the closeout history, one row per window.
	claudePlanWeeksFile = "weeks.jsonl"

	// claudePlanMaxWeekLineBytes caps one history row; a real row is under a hundred bytes.
	claudePlanMaxWeekLineBytes = 64 << 10

	// claudePlanTrimSlackDays is how far past the retention the oldest sample
	// must be before the series is rewritten, so the rewrite runs about once a
	// week rather than on every poll once the series is full.
	claudePlanTrimSlackDays = 7
)

// ClaudePlanWeek is one weekly window as the rollup reads it.
type ClaudePlanWeek struct {
	// StartAt and ResetsAt bound the window in epoch seconds; ResetsAt identifies it.
	StartAt  int64 `json:"start_at"`
	ResetsAt int64 `json:"resets_at"`
	// FinalPct is the window's last reading: where it ended, or where it stands
	// while InProgress.
	FinalPct float64 `json:"final_pct"`
	// InProgress marks the window the newest reading is in. It is counted in the
	// month but never recorded, since its percentage is not final.
	InProgress bool `json:"in_progress,omitempty"`
}

// ClaudePlanMonth is the billing month's rollup, embedded in ClaudePlan so the
// page reads week and month off one payload.
type ClaudePlanMonth struct {
	// StartAt and EndAt are the billing month's bounds in epoch seconds.
	StartAt int64 `json:"month_start_at"`
	EndAt   int64 `json:"month_end_at"`
	// ValueUsedUSD is the month's share of its weeks' value, and UtilizationPct
	// that over the plan's price. Both stay zero when no week is counted.
	ValueUsedUSD   float64 `json:"month_value_used_usd"`
	UtilizationPct float64 `json:"month_utilization_pct"`
	// WeeksAtCeiling counts whole weeks that reached ClaudePlanCeilingPct, and
	// AvgWeeklyPct is the mean of the month's weeks.
	WeeksAtCeiling int     `json:"weeks_at_ceiling"`
	AvgWeeklyPct   float64 `json:"avg_weekly_pct"`
	// WeeksCounted is zero when the history holds no week of the month; the page
	// reads that as no month to report, never as 0% of the plan.
	WeeksCounted int `json:"month_weeks"`
	// OpenWeek says a counted week is still running.
	OpenWeek bool `json:"month_open_week"`
	// PrevUtilizationPct is the previous billing month's utilization, null when
	// the history holds none of its weeks.
	PrevUtilizationPct *float64 `json:"prev_month_utilization_pct"`
}

// ComputeClaudePlanMonth folds weeks into the rollup of the billing month
// containing now, pricing them against planUSDMonth. Dollars are prorated by the
// days of each window inside the month; the ceiling count and the mean take
// whole windows.
func ComputeClaudePlanMonth(weeks []ClaudePlanWeek, now time.Time, planUSDMonth float64, renewalDay int) ClaudePlanMonth {
	price := planUSD(planUSDMonth)
	weeklyUSD := roundTo2(price * claudePlanWeekDays / claudePlanMonthDays)
	start, end := claudePlanBillingMonth(now, renewalDay)
	m := ClaudePlanMonth{StartAt: start.Unix(), EndAt: end.Unix()}

	month := claudePlanMonthFold(weeks, start, end, weeklyUSD)
	m.ValueUsedUSD, m.WeeksCounted, m.WeeksAtCeiling = month.valueUSD, month.weeks, month.atCeiling
	m.AvgWeeklyPct, m.OpenWeek = month.avgPct, month.openWeek
	if m.WeeksCounted > 0 {
		m.UtilizationPct = roundTo2(m.ValueUsedUSD / price * 100)
	}

	// The previous month is the one that ends where this one starts, so the two
	// tile whatever the renewal day is.
	prevStart, _ := claudePlanBillingMonth(start.Add(-time.Second), renewalDay)
	if prev := claudePlanMonthFold(weeks, prevStart, start, weeklyUSD); prev.weeks > 0 {
		pct := roundTo2(prev.valueUSD / price * 100)
		m.PrevUtilizationPct = &pct
	}
	return m
}

// claudePlanMonthTotals is one billing month's share of the weeks.
type claudePlanMonthTotals struct {
	valueUSD  float64
	weeks     int
	atCeiling int
	avgPct    float64
	openWeek  bool
}

// claudePlanMonthFold folds the weeks with a day inside [start, end). A window
// of no length has no day in any month and drops out here.
func claudePlanMonthFold(weeks []ClaudePlanWeek, start, end time.Time, weeklyUSD float64) claudePlanMonthTotals {
	var t claudePlanMonthTotals
	var sumPct float64
	for _, w := range weeks {
		share := claudePlanWindowShare(w, start, end)
		if share <= 0 {
			continue
		}
		t.valueUSD += w.FinalPct / claudePlanLimitPct * weeklyUSD * share
		t.weeks++
		sumPct += w.FinalPct
		if w.FinalPct >= ClaudePlanCeilingPct {
			t.atCeiling++
		}
		t.openWeek = t.openWeek || w.InProgress
	}
	t.valueUSD = roundTo2(t.valueUSD)
	if t.weeks > 0 {
		t.avgPct = roundTo2(sumPct / float64(t.weeks))
	}
	return t
}

// claudePlanWindowShare is the fraction of a window's span inside [start, end).
func claudePlanWindowShare(w ClaudePlanWeek, start, end time.Time) float64 {
	span := w.ResetsAt - w.StartAt
	if span <= 0 {
		return 0
	}
	from, to := max(w.StartAt, start.Unix()), min(w.ResetsAt, end.Unix())
	if to <= from {
		return 0
	}
	return float64(to-from) / float64(span)
}

// claudePlanBillingMonth is the span between the renewal instant at or before
// now and the next one.
func claudePlanBillingMonth(now time.Time, renewalDay int) (start, end time.Time) {
	start = claudePlanRenewalOnOrBefore(now, renewalDay)
	return start, claudePlanRenewalAfter(start, renewalDay)
}

// claudePlanRenewalOnOrBefore is the renewal midnight at or before now.
func claudePlanRenewalOnOrBefore(now time.Time, renewalDay int) time.Time {
	y, m, _ := now.Date()
	if this := claudePlanRenewalIn(y, m, renewalDay, now.Location()); !this.After(now) {
		return this
	}
	return claudePlanRenewalIn(y, m-1, renewalDay, now.Location())
}

// claudePlanRenewalAfter is the first renewal midnight strictly after t, so a
// month never ends on the instant it starts.
func claudePlanRenewalAfter(t time.Time, renewalDay int) time.Time {
	y, m, _ := t.Date()
	if this := claudePlanRenewalIn(y, m, renewalDay, t.Location()); this.After(t) {
		return this
	}
	return claudePlanRenewalIn(y, m+1, renewalDay, t.Location())
}

// claudePlanRenewalIn is the renewal midnight in the given calendar month, on
// its last day when the month is shorter than renewalDay.
func claudePlanRenewalIn(year int, month time.Month, renewalDay int, loc *time.Location) time.Time {
	day := planRenewalDay(renewalDay)
	if last := time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day(); day > last {
		day = last
	}
	return time.Date(year, month, day, 0, 0, 0, 0, loc)
}

// planRenewalDay falls back to the default for a day no month has.
func planRenewalDay(day int) int {
	if day < 1 || day > 31 {
		return ClaudePlanDefaultRenewalDay
	}
	return day
}

// claudePlanWeeksFromSamples folds the samples to one window per reset instant,
// oldest first, each ending at its last percentage.
func claudePlanWeeksFromSamples(samples []ClaudePlanSample) []ClaudePlanWeek {
	var weeks []ClaudePlanWeek
	for _, resetsAt := range claudePlanWindowInstants(samples) {
		window := claudePlanWindow(samples, resetsAt)
		if len(window) == 0 {
			continue
		}
		newest := window[len(window)-1]
		weeks = append(weeks, ClaudePlanWeek{
			StartAt:  newest.SevenDayResetsAt - claudePlanWeekSecs,
			ResetsAt: newest.SevenDayResetsAt,
			FinalPct: newest.SevenDay,
		})
	}
	return weeks
}

// claudePlanWindowInstants is the distinct window reset instants in the samples,
// ascending; readings within claudePlanWindowSlackSecs name one instant.
func claudePlanWindowInstants(samples []ClaudePlanSample) []int64 {
	var instants []int64
	for _, s := range samples {
		if s.SevenDayResetsAt <= 0 {
			continue
		}
		known := false
		for _, have := range instants {
			if claudePlanSameWindow(have, s.SevenDayResetsAt) {
				known = true
				break
			}
		}
		if !known {
			instants = append(instants, s.SevenDayResetsAt)
		}
	}
	sort.Slice(instants, func(i, j int) bool { return instants[i] < instants[j] })
	return instants
}

// claudePlanWeeks is the history the rollup reads: the recorded windows plus the
// window the newest reading is in. Windows whose reset has passed are recorded
// on the first poll that meets them.
//
// The returned error says the history could not be read or a closeout could not
// be written; the weeks are still the best the poll can answer. An unreadable
// history is never appended to, since every window in the samples would look new.
func (r *ClaudePlanReader) claudePlanWeeks(samples []ClaudePlanSample, now time.Time) ([]ClaudePlanWeek, error) {
	history, readErr := readClaudePlanWeeks(r.historyPath)
	weeks := claudePlanWeeksFromSamples(samples)
	if carried, ok := claudePlanCarriedWeek(samples, weeks, history, now); ok {
		weeks = append(weeks, carried)
	}
	var fresh []ClaudePlanWeek
	for _, w := range weeks {
		if claudePlanHasWeek(history, w) {
			continue
		}
		switch {
		case w.ResetsAt > now.Unix():
			w.InProgress = true
		case readErr == nil:
			fresh = append(fresh, w)
		}
		history = append(history, w)
	}
	if len(fresh) == 0 {
		return history, readErr
	}
	return history, errors.Join(readErr, appendClaudePlanWeeks(r.historyPath, fresh))
}

// claudePlanCarriedWeek is the window the tail no longer names: the week before
// the newest one the samples name, when its reset has passed and the history
// stops short of it. Readings age out of the bounded tail read, so a poll that
// first meets a reset a week or more after its last one otherwise never closes
// the week in between and the month loses it (gt-qt4os).
//
// An empty history carries nothing: with no recorded week there is no hole to
// fill, only a series that may have started inside the week.
func claudePlanCarriedWeek(samples []ClaudePlanSample, weeks, history []ClaudePlanWeek, now time.Time) (ClaudePlanWeek, bool) {
	if len(weeks) == 0 || len(history) == 0 {
		return ClaudePlanWeek{}, false
	}
	resetsAt := weeks[len(weeks)-1].ResetsAt - claudePlanWeekSecs
	if resetsAt > now.Unix() ||
		claudePlanHasWeek(weeks, ClaudePlanWeek{ResetsAt: resetsAt}) ||
		claudePlanHasWeek(history, ClaudePlanWeek{ResetsAt: resetsAt}) {
		return ClaudePlanWeek{}, false
	}
	var recorded int64
	for _, w := range history {
		recorded = max(recorded, w.ResetsAt)
	}
	if recorded >= resetsAt {
		return ClaudePlanWeek{}, false
	}
	return ClaudePlanWeek{
		StartAt:  resetsAt - claudePlanWeekSecs,
		ResetsAt: resetsAt,
		FinalPct: claudePlanReadingAround(samples, resetsAt),
	}, true
}

// claudePlanReadingAround is the percentage of the newest reading at or before
// instant, and of the oldest reading when every reading came after it, so a
// window the tail has emptied still closes at the reading nearest it.
func claudePlanReadingAround(samples []ClaudePlanSample, instant int64) float64 {
	var before, after *ClaudePlanSample
	for i := range samples {
		s := &samples[i]
		if s.T <= instant {
			if before == nil || s.T > before.T {
				before = s
			}
			continue
		}
		if after == nil || s.T < after.T {
			after = s
		}
	}
	if before != nil {
		return before.SevenDay
	}
	if after != nil {
		return after.SevenDay
	}
	return 0
}

// claudePlanHasWeek reports whether weeks holds the window.
func claudePlanHasWeek(weeks []ClaudePlanWeek, w ClaudePlanWeek) bool {
	for _, have := range weeks {
		if claudePlanSameWindow(have.ResetsAt, w.ResetsAt) {
			return true
		}
	}
	return false
}

// readClaudePlanWeeks decodes the whole history, skipping a row that will not
// parse or names no window. A second row for a window already read is skipped
// too, so two pollers that both recorded a closeout count it once.
func readClaudePlanWeeks(path string) ([]ClaudePlanWeek, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), claudePlanMaxWeekLineBytes)
	var weeks []ClaudePlanWeek
	for sc.Scan() {
		var w ClaudePlanWeek
		if err := json.Unmarshal(sc.Bytes(), &w); err != nil || w.ResetsAt <= 0 || claudePlanHasWeek(weeks, w) {
			continue
		}
		weeks = append(weeks, w)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return weeks, nil
}

// appendClaudePlanWeeks adds the rows to the history in one O_APPEND write, so
// rows from concurrent appenders interleave whole.
func appendClaudePlanWeeks(path string, weeks []ClaudePlanWeek) error {
	var buf bytes.Buffer
	for _, week := range weeks {
		line, err := json.Marshal(week)
		if err != nil {
			return err
		}
		buf.Write(append(line, '\n'))
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// claudePlanWeeksPath is the history beside the series at samplesPath.
func claudePlanWeeksPath(samplesPath string) string {
	return filepath.Join(filepath.Dir(samplesPath), claudePlanWeeksFile)
}

// trimClaudePlanSamples rewrites the series without the lines older than cutoff
// once its oldest line is claudePlanTrimSlackDays past it, and does nothing
// before then.
//
// The status line appends to the same path, so the rewrite is a rename. An
// append that opened the old file before the rename lands in the old inode,
// which f still holds: the bytes f yields after the rename are re-appended to
// the new file. Only a write delayed past that read is lost.
func trimClaudePlanSamples(path string, cutoff time.Time) error {
	return trimClaudePlanSamplesHook(path, cutoff, nil)
}

// trimClaudePlanSamplesHook is trimClaudePlanSamples with beforeRewrite run
// between the read and the rewrite, so a test can append in that window.
func trimClaudePlanSamplesHook(path string, cutoff time.Time, beforeRewrite func()) error {
	oldest, ok, err := oldestClaudePlanSample(path)
	if err != nil {
		return err
	}
	if !ok || oldest >= cutoff.Add(-claudePlanTrimSlackDays*24*time.Hour).Unix() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var kept []byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), claudePlanMaxLineBytes)
	for sc.Scan() {
		var s ClaudePlanSample
		// An undatable line is a torn append and goes with the old ones.
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil || s.T < cutoff.Unix() {
			continue
		}
		kept = append(append(kept, sc.Bytes()...), '\n')
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if beforeRewrite != nil {
		beforeRewrite()
	}
	if err := atomicfile.WriteFile(path, kept, info.Mode().Perm()); err != nil {
		return err
	}
	orphaned, err := io.ReadAll(f)
	if err != nil || len(orphaned) == 0 {
		return err
	}
	out, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := out.Write(orphaned); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// oldestClaudePlanSample is the instant of the series' first dated line, read
// from the head of the file, and false when the head holds none. A missing
// series is an error, not an empty one.
func oldestClaudePlanSample(path string) (int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, claudePlanTailBytes))
	sc.Buffer(make([]byte, 0, 4096), claudePlanMaxLineBytes)
	for sc.Scan() {
		var s ClaudePlanSample
		if err := json.Unmarshal(sc.Bytes(), &s); err == nil && s.T > 0 {
			return s.T, true, nil
		}
	}
	return 0, false, sc.Err()
}
