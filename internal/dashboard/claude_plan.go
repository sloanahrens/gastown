package dashboard

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// The Claude plan block: what the Claude subscription's weekly and five-hour
// limits stand at, folded from the samples the status line appends to
// <town>/.runtime/claude-plan/samples.jsonl. It is the Claude twin of the
// DeepSeek spend numbers on the Cost panel, so the operator reads both limits
// side by side (gt-9drev).
//
// The series is written by ~/.claude/statusline-command.sh, one JSON line per
// change: t, five_hour, five_hour_resets_at, seven_day, seven_day_resets_at,
// percentages 0-100 and resets as epoch seconds. A poll reads the tail, and a
// line it cannot parse is skipped: a series with one garbled append still
// measures the week.
//
// The reader also writes, to files under <town>/.runtime/claude-plan/ only: the
// weekly closeouts the month rollup reads, and the trim that keeps the status
// line's series to claudePlanRetentionDays. The series is the status line's
// file, so the trim is the one place the dashboard rewrites another writer's
// data (gt-bcq4m).

const (
	// claudePlanStale is how long after its newest sample the series reads as
	// stale. The status line appends a sample when a reading moves or ten
	// minutes after the last one, so half an hour is several missed appends and
	// the meter is no longer what the page can call current.
	claudePlanStale = 30 * time.Minute

	// claudePlanTailBytes bounds what one poll reads. At most one sample
	// arrives every ten minutes, so this covers days of series, and the math
	// reaches back no further than the current weekly window; that bound is also
	// why no mtime cache sits in front of the read.
	claudePlanTailBytes = 256 << 10

	// claudePlanMaxLineBytes is the cap on one sample line. A real line is under
	// two hundred bytes, so a longer one is not this series.
	claudePlanMaxLineBytes = 64 << 10

	// ClaudePlanDefaultUSD is the monthly price the meter draws against when no
	// price is configured.
	ClaudePlanDefaultUSD = 200

	// claudePlanMonthDays is the mean month: the plan price buys a share of the
	// limit per day, so the week the meter reports is a week of that division.
	claudePlanMonthDays = 30.4

	// claudePlanWeekDays is the span the weekly limit covers, and
	// claudePlanLimitPct the percentage at which it is spent.
	claudePlanWeekDays = 7
	claudePlanLimitPct = 100.0

	// claudePlanDaySecs is a day, the unit both paces are per.
	claudePlanDaySecs = 86400

	// claudePlanWeekSecs is the weekly window's span; a window starts a week
	// before the instant it resets.
	claudePlanWeekSecs = claudePlanWeekDays * claudePlanDaySecs

	// claudePlanWindowSlackSecs is how far apart two readings' reset instants
	// may be and still name the same window. A window resets at one instant,
	// which the status line writes as whole seconds of a countdown the API
	// recomputes, so two readings of it agree within seconds; two windows are
	// seven days apart, so this slack never merges them.
	claudePlanWindowSlackSecs = 60

	// claudePlanRetentionDays is how long the raw series is kept; the weekly
	// history outlives it.
	claudePlanRetentionDays = 90
)

// ClaudePlanSample is one line of the status line's series, written with the
// field names the status line uses so a line decodes straight into it.
type ClaudePlanSample struct {
	// T is when the reading was taken, in epoch seconds.
	T int64 `json:"t"`
	// FiveHour and SevenDay are the percentages of each limit in use.
	FiveHour float64 `json:"five_hour"`
	SevenDay float64 `json:"seven_day"`
	// FiveHourResetsAt and SevenDayResetsAt are when each limit rolls over, in
	// epoch seconds; zero is a reading that carried none.
	FiveHourResetsAt int64 `json:"five_hour_resets_at"`
	SevenDayResetsAt int64 `json:"seven_day_resets_at"`
}

// ClaudePlan is the block's payload, drawn as the DeepSeek spend line's twin.
type ClaudePlan struct {
	// WeeklyPct and FiveHourPct are the newest reading's percentages of each
	// limit.
	WeeklyPct   float64 `json:"weekly_pct"`
	FiveHourPct float64 `json:"five_hour_pct"`
	// WeeklyResetsAt and FiveHourResetsAt are when each limit rolls over, in
	// epoch seconds; zero when the newest sample carried none.
	WeeklyResetsAt   int64 `json:"weekly_resets_at"`
	FiveHourResetsAt int64 `json:"five_hour_resets_at"`
	// PacePerDay1h and PacePerDay24h are the weekly limit's percentage points
	// per day over the last hour and the last day, measured across the current
	// window's samples only: a rollover drops the percentage, and a pace drawn
	// across one would read as a fall.
	PacePerDay1h  float64 `json:"pace_per_day_1h"`
	PacePerDay24h float64 `json:"pace_per_day_24h"`
	// ProjectedPctAtReset is where the weekly percentage lands when the limit
	// rolls over, at the day's pace. It passes claudePlanLimitPct for a week
	// that will run out before it resets.
	ProjectedPctAtReset float64 `json:"projected_pct_at_reset"`
	// HitsLimitInDays is how long the weekly limit lasts at the day's pace, zero
	// for a week already at the limit, and null when the pace is flat or falling
	// or when the limit outlives the window and so is never hit.
	HitsLimitInDays *float64 `json:"hits_limit_in_days"`
	// PlanUSDMonth is the monthly price the percentages are drawn against,
	// WeeklyValueUSD the share of it one week of the limit is worth, and
	// UsedValueUSD the approximate dollars of that week already spent.
	PlanUSDMonth   float64 `json:"plan_usd_month"`
	WeeklyValueUSD float64 `json:"weekly_value_usd"`
	UsedValueUSD   float64 `json:"used_value_usd"`
	// APIEquivalentWeekUSD and APIEquivalentMonthUSD are what the Claude tokens
	// the town spent over the plan's week and the billing month would cost at
	// Anthropic's list prices (claude_prices.json), so the page can lay the
	// metered bill beside the flat price the plan charges. Both are null when
	// the usage records could not be read, which the page says nothing about
	// rather than drawing a month of no spend as $0.00 (gt-d8sem).
	APIEquivalentWeekUSD  *float64 `json:"api_equivalent_week_usd,omitempty"`
	APIEquivalentMonthUSD *float64 `json:"api_equivalent_month_usd,omitempty"`
	// Unpriced names the Claude models in those windows the price table cannot
	// price; they count zero, so the figures beside this list are a floor.
	Unpriced []string `json:"unpriced,omitempty"`
	// SampledAt is the newest sample's instant, in epoch seconds.
	SampledAt int64 `json:"sampled_at"`
	// Stale says the newest sample is older than claudePlanStale, that the series
	// has none, or that its window has no end after now to be measured against.
	Stale bool `json:"stale"`
	// Unavailable says the series could not be read, which the page draws as
	// itself and never as a week at 0%.
	Unavailable bool `json:"unavailable,omitempty"`
	// StateError names what the poll could not do: the closeout history or trim
	// write that failed, or the price table the API-equivalent figures could not
	// use. The numbers above are still the poll's best answer; the page shows the
	// error on hover, so a history that stopped growing is not mistaken for a
	// healthy one, and a missing figure is not read as a month of no spend.
	StateError string `json:"state_error,omitempty"`

	// ClaudePlanMonth is the billing month's rollup (gt-bcq4m).
	ClaudePlanMonth

	// Samples is the series this read returned, kept so the Cost charts' Claude
	// dollars are folded from the same read rather than a second one that could
	// fail on its own. It stays off the wire: the charts carry what the page
	// draws.
	Samples PlanSamples `json:"-"`
}

// ClaudePlanReader reads a status line series from the path it was given, and
// keeps the weekly history and the series' trim that live beside it. Reads are
// serialized, so concurrent callers in one process record a closeout once.
type ClaudePlanReader struct {
	mu           sync.Mutex
	path         string
	historyPath  string
	planUSDMonth float64
	// usage is where the API-equivalent figures' tokens come from: the usage
	// scan's records, read at the same instant the week is measured at. A reader
	// without one draws the plan line alone.
	usage func(now time.Time) []UsageRecord
	// prices is the table the figures' rates come from, read on each poll so a
	// table this process cannot decode is reported rather than assumed away. It
	// is EmbeddedClaudePrices unless a test points it elsewhere.
	prices     func() (*ClaudePriceTable, error)
	renewalDay int
}

// NewClaudePlanReader returns a reader of the series at path, pricing its
// percentages against planUSDMonth, which falls back to ClaudePlanDefaultUSD
// when it is not a price. The plan renews on ClaudePlanDefaultRenewalDay unless
// WithRenewalDay says otherwise.
func NewClaudePlanReader(path string, planUSDMonth float64) *ClaudePlanReader {
	return &ClaudePlanReader{
		path:         path,
		historyPath:  claudePlanWeeksPath(path),
		planUSDMonth: planUSDMonth,
		prices:       EmbeddedClaudePrices,
		renewalDay:   ClaudePlanDefaultRenewalDay,
	}
}

// WithRenewalDay sets the day the billing month starts on; a day no month has
// falls back to the default.
func (r *ClaudePlanReader) WithRenewalDay(day int) *ClaudePlanReader {
	r.renewalDay = planRenewalDay(day)
	return r
}

// WithUsage points the reader at the usage scan's records, so its payload also
// carries what the town's Claude tokens would cost at API list prices. The
// source is called with the same now the plan is measured at, so the two
// windows cannot be drawn from different clocks. A source that answers nil is a
// record set that could not be read, which leaves the figures unset; an empty
// set is a town that spent nothing, which is a figure of zero.
func (r *ClaudePlanReader) WithUsage(records func(now time.Time) []UsageRecord) *ClaudePlanReader {
	r.usage = records
	return r
}

// Read returns the block's payload from the series as it stands at now: a file
// that is missing or unreadable is an unavailable block, never an empty week.
//
// The poll also records windows whose reset has passed and trims the series. A
// failure of either leaves the week and month as drawn and is reported in
// StateError, so a town whose .runtime is not writable still draws its week.
func (r *ClaudePlanReader) Read(now time.Time) *ClaudePlan {
	r.mu.Lock()
	defer r.mu.Unlock()
	samples, err := readClaudePlanSamples(r.path)
	var p ClaudePlan
	var stateErrs []error
	if err != nil {
		p = ClaudePlan{PlanUSDMonth: planUSD(r.planUSDMonth), Stale: true, Unavailable: true}
	} else {
		if err := trimClaudePlanSamples(r.path, now.Add(-claudePlanRetentionDays*24*time.Hour)); err != nil {
			stateErrs = append(stateErrs, fmt.Errorf("series trim: %w", err))
		}
		p = ComputeClaudePlan(samples, now, r.planUSDMonth)
		if err := r.attachAPIEquivalent(&p, now); err != nil {
			stateErrs = append(stateErrs, err)
		}
	}
	p.Samples = costSamples(samples, err)
	// The history outlives the series, so an unreadable series still reports the
	// months already recorded.
	weeks, err := r.claudePlanWeeks(samples, now)
	if err != nil {
		stateErrs = append(stateErrs, fmt.Errorf("weekly history: %w", err))
	}
	p.ClaudePlanMonth = ComputeClaudePlanMonth(weeks, now, r.planUSDMonth, r.renewalDay)
	if err := errors.Join(stateErrs...); err != nil {
		p.StateError = strings.ReplaceAll(err.Error(), "\n", "; ")
	}
	return &p
}

// costSamples is a poll's read of the series as the Cost charts' Claude dollars
// need it: the samples, and whether the read failed. A failed read is reported
// rather than drawn as a series of zeros. A file that is not there yet is a
// status line that has not written a sample, which the charts say instead of a
// failure.
func costSamples(samples []ClaudePlanSample, err error) PlanSamples {
	switch {
	case err == nil:
		return PlanSamples{Samples: samples, Read: true}
	case errors.Is(err, fs.ErrNotExist):
		return PlanSamples{Read: true}
	}
	return PlanSamples{Read: true, Failed: true}
}

// attachAPIEquivalent joins the API-equivalent figures to a payload the series
// answered. Records that cannot be read leave both figures unset and are not an
// error: a figure of zero there would be the page's own lie, since "nothing
// ran" and "the records are gone" are different months (gt-xyzs8, gt-d8sem).
//
// A price table it cannot use is an error instead, so the two are not one
// silence. A table that will not decode leaves the figures unset, and one that
// has aged past claudePricesMaxAge prices them from rates nobody has refreshed;
// the caller reports either on the block, where the page draws it.
func (r *ClaudePlanReader) attachAPIEquivalent(p *ClaudePlan, now time.Time) error {
	if r.usage == nil {
		return nil
	}
	recs := r.usage(now)
	if recs == nil {
		return nil
	}
	prices, err := r.prices()
	if err != nil {
		return fmt.Errorf("price table: %w", err)
	}
	eq := APIEquivalent(recs, prices, now, p.WeeklyResetsAt)
	week, month := eq.WeekUSD, eq.MonthUSD
	p.APIEquivalentWeekUSD, p.APIEquivalentMonthUSD = &week, &month
	p.Unpriced = eq.Unpriced
	// The figures stand as the poll's best answer and the age goes beside them,
	// so a figure drawn from last quarter's rates is not read as a current one.
	if age, aged := prices.Aged(now); aged {
		return fmt.Errorf("prices as of %s, %s old: re-read the list prices", prices.AsOf, age.Round(24*time.Hour))
	}
	return nil
}

// ComputeClaudePlan folds the samples into the payload the page draws.
//
// Only two things hang off now: how old the newest sample is, and how long the
// window has left to run. The pace is measured between samples, so a series an
// hour behind still reports the rate it saw rather than one diluted by the
// silence.
func ComputeClaudePlan(samples []ClaudePlanSample, now time.Time, planUSDMonth float64) ClaudePlan {
	p := ClaudePlan{PlanUSDMonth: planUSD(planUSDMonth)}
	// The week's dollars follow the price rather than the series, so they are
	// there for a page that draws a plan nobody has measured yet.
	p.WeeklyValueUSD = roundTo2(p.PlanUSDMonth * claudePlanWeekDays / claudePlanMonthDays)
	if len(samples) == 0 {
		p.Stale = true
		return p
	}

	newest := samples[0]
	for _, s := range samples {
		if s.T > newest.T {
			newest = s
		}
	}
	p.WeeklyPct, p.FiveHourPct = newest.SevenDay, newest.FiveHour
	p.WeeklyResetsAt, p.FiveHourResetsAt = newest.SevenDayResetsAt, newest.FiveHourResetsAt
	p.SampledAt = newest.T
	// A reading the meter cannot place in the current window is not the current
	// week: with no reset instant, or one the clock has already passed, the
	// percentage describes a week nobody is spending against now. Zero is the
	// absent instant and reads the same way.
	windowEnded := newest.SevenDayResetsAt <= now.Unix()
	p.Stale = windowEnded || now.Sub(time.Unix(newest.T, 0)) > claudePlanStale
	p.UsedValueUSD = roundTo2(newest.SevenDay / claudePlanLimitPct * p.WeeklyValueUSD)

	window := claudePlanWindow(samples, newest.SevenDayResetsAt)
	pace1h := claudePlanPace(window, time.Hour)
	pace24h := claudePlanPace(window, 24*time.Hour)
	p.PacePerDay1h, p.PacePerDay24h = roundTo2(pace1h), roundTo2(pace24h)

	daysLeft := float64(newest.SevenDayResetsAt-now.Unix()) / claudePlanDaySecs
	if daysLeft < 0 {
		daysLeft = 0
	}
	p.ProjectedPctAtReset = roundTo2(newest.SevenDay + pace24h*daysLeft)
	// A week already at the limit has nothing left to spend, so it is reached at
	// no pace at all; below it, the day's pace says when, and a pace that is flat
	// or falling never gets there. A limit that outlasts the window is the
	// reset's, not the meter's, so the field stays null.
	switch {
	case newest.SevenDay >= claudePlanLimitPct:
		hits := 0.0
		p.HitsLimitInDays = &hits
	case pace24h > 0:
		if d := (claudePlanLimitPct - newest.SevenDay) / pace24h; d <= daysLeft {
			hits := roundTo2(d)
			p.HitsLimitInDays = &hits
		}
	}
	return p
}

// claudePlanWindow is the samples of the newest reading's own window, in time
// order: the limit's resets_at identifies a run of the limit, and a sample from
// a run that has ended belongs to a week nobody is spending against now.
func claudePlanWindow(samples []ClaudePlanSample, resetsAt int64) []ClaudePlanSample {
	window := make([]ClaudePlanSample, 0, len(samples))
	for _, s := range samples {
		if claudePlanSameWindow(s.SevenDayResetsAt, resetsAt) {
			window = append(window, s)
		}
	}
	sort.Slice(window, func(i, j int) bool { return window[i].T < window[j].T })
	return window
}

// claudePlanSameWindow reports whether two reset instants name one window. The
// match allows claudePlanWindowSlackSecs either way, so a reset instant the API
// reports a few seconds differently does not split one window into two single
// readings and leave the pace reading zero; two windows are a week apart, so the
// slack never merges two of them.
func claudePlanSameWindow(a, b int64) bool {
	d := a - b
	return -claudePlanWindowSlackSecs <= d && d <= claudePlanWindowSlackSecs
}

// claudePlanPace is the weekly percentage's rise per day over span, measured
// from the window's newest sample back to the newest sample at least span old.
// A window younger than span is measured from its oldest sample instead, the
// longest span the window can offer, and a window of one sample is no span at
// all: a flat pace of zero.
func claudePlanPace(window []ClaudePlanSample, span time.Duration) float64 {
	if len(window) < 2 {
		return 0
	}
	newest, base := window[len(window)-1], window[0]
	cutoff := newest.T - int64(span/time.Second)
	for _, s := range window {
		if s.T <= cutoff {
			base = s
		}
	}
	elapsed := float64(newest.T - base.T)
	if elapsed <= 0 {
		return 0
	}
	return (newest.SevenDay - base.SevenDay) / (elapsed / claudePlanDaySecs)
}

// readClaudePlanSamples decodes the series from the tail of the file. A line
// that will not parse is skipped rather than failing the read: the seek to the
// tail cuts the first line it lands in, and an append cut short leaves one bad
// line behind.
func readClaudePlanSamples(path string) ([]ClaudePlanSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > claudePlanTailBytes {
		if _, err := f.Seek(info.Size()-claudePlanTailBytes, io.SeekStart); err != nil {
			return nil, err
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), claudePlanMaxLineBytes)
	var samples []ClaudePlanSample
	for sc.Scan() {
		var s ClaudePlanSample
		// A sample with no instant cannot be placed against any other, so a line
		// without one is as useless as a line that will not parse.
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil || s.T <= 0 {
			continue
		}
		samples = append(samples, s)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return samples, nil
}

// planUSD is the price the meter draws against: the configured one, or the
// default when the operator set none. A price that is not a finite positive
// number is no price, and the default stands in its place: a NaN or an
// infinity would reach the page as dollars, and the JSON encoder refuses to
// write it, taking the rest of the state down with the one field.
func planUSD(planUSDMonth float64) float64 {
	if math.IsNaN(planUSDMonth) || math.IsInf(planUSDMonth, 0) || planUSDMonth <= 0 {
		return ClaudePlanDefaultUSD
	}
	return planUSDMonth
}

// roundTo2 is the payload's precision: the page draws one decimal place, and
// two keep float noise out of the JSON.
func roundTo2(v float64) float64 {
	r := math.Round(v*100) / 100
	if r == 0 {
		return 0
	}
	return r
}
