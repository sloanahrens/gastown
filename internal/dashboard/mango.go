package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// The Mango panel: the town-metrics report on which predictors of a landing
// outcome replicated on the holdout. panel.json comes from a Python process
// outside the dashboard, so the reader trusts nothing in it: every string is
// stripped and capped before it reaches the page, a file the reader cannot make
// sense of is a named error rather than a guess, and the read opens the file for
// reading only and writes nothing.

const (
	mangoPanelFile = "panel.json"

	// mangoSchema is the panel shape this reader knows. A file naming another
	// version is an error state: its fields are not this panel's to guess at.
	mangoSchema = 1

	// mangoPanelMaxBytes refuses a larger panel before reading it, so a file
	// that is not one of the writer's cannot be read into memory to be
	// rejected. The writer's own panels run a few kilobytes.
	mangoPanelMaxBytes = 256 << 10

	// mangoStale is the age past which the panel reads as stale. The analysis
	// is rebuilt daily, so this leaves a day of missed rebuilds before the page
	// calls the panel old.
	mangoStale = 36 * time.Hour

	// The cap on each string the page shows, in runes. A target is an id and a
	// best box is one line of column constraints.
	mangoTargetMax = 40
	mangoBoxMax    = 240

	// mangoLabelMax caps a target's plain-words title, which the page draws in
	// the column a target id used to fill.
	mangoLabelMax = 60

	// The caps on the panel's lists, in entries. The writer trims each list to
	// the same count; the reader trims again because a panel from another hand
	// is not the writer's, and a row taller than the page has room for. A dated
	// list reads oldest first, so its cap keeps the newest entries.
	mangoConditionsMax = 16
	mangoWeeklyMax     = 26
	mangoEventsMax     = 16

	// mangoFreeTextMax caps a condition's text and an event's what, the two
	// strings the page draws whose length the writer does not bound, in runes.
	mangoFreeTextMax = 120

	// mangoDayLayout is the writer's date format for a week, an event and a
	// target's last positive: a calendar day with no time of day.
	mangoDayLayout = "2006-01-02"
)

// The reasons the writer gives a not-yet target. The page has a sentence for
// each; any other word is dropped rather than drawn.
const (
	mangoReasonFewPositives       = "few_positives"
	mangoReasonNoHoldoutPositives = "no_holdout_positives"
)

// Mango states. Fresh and stale are a panel generated within mangoStale of the
// read or older, missing is a panel the writer has not produced yet, and error
// is one the reader could not use, named in Mango.Note.
const (
	MangoFresh   = "fresh"
	MangoStale   = "stale"
	MangoMissing = "missing"
	MangoError   = "error"
)

// mangoNoPanel is the sentence the panel carries before its first collection.
const mangoNoPanel = "not collected yet"

// mangoNoPanelRead is the sentence for a reader that hands the hub no panel at
// all, which is a state the reader did not name itself.
const mangoNoPanelRead = "the panel reader returned no panel"

// mangoOversize names the size cap the way the page shows it. The number is
// read out of the cap itself, so the sentence and the reader that enforces it
// cannot come apart.
var mangoOversize = fmt.Sprintf("the panel is larger than %d KiB", mangoPanelMaxBytes>>10)

// Mango verdicts, the writer's three words for a target's best box: earning
// replicated on the holdout, not-yet had too few positives to check, and
// no-signal found nothing that replicated.
const (
	MangoEarning  = "earning"
	MangoNoSignal = "no-signal"
	MangoNotYet   = "not-yet"
)

var errMangoOversize = errors.New("mango: panel over the size cap")

// mangoPanel is the shape of the file the writer produces. The key names are
// the writer's own: a panel from another shape is this reader's error state,
// not a set of fields to rename here.
type mangoPanel struct {
	SchemaVersion int           `json:"schema_version"`
	GeneratedAt   string        `json:"generated_at"`
	Items         int           `json:"items"`
	SnapshotHours int           `json:"snapshot_hours"`
	Weekly        []mangoWeek   `json:"weekly"`
	Events        []mangoEvent  `json:"events"`
	Targets       []mangoTarget `json:"targets"`
}

// mangoWeek is one week of the panel's trend as the file writes it: the landings
// that week across every rig and the share of them rejected before landing.
type mangoWeek struct {
	Week          string  `json:"week"`
	Landings      int     `json:"landings"`
	RejectedShare float64 `json:"rejected_share"`
}

// mangoEvent is one regime event the trend is marked with, as the file writes
// it: the day the regime changed and what changed.
type mangoEvent struct {
	Date string `json:"date"`
	What string `json:"what"`
}

// mangoCondition is one range of a target's best box read alone, as the file
// writes it: the range's text, the share of the prepared landings it holds by
// itself, and whether that share is small enough for the page to draw it as a
// chip.
type mangoCondition struct {
	Text    string  `json:"text"`
	Share   float64 `json:"share"`
	Narrows bool    `json:"narrows"`
}

// mangoTarget is one target as the file writes it. A target skipped for too few
// positives carries its name and its positives with no box, and the writer's
// not-yet verdict. The box's counts, interval, thin flag and conditions, and
// the target's label, reason and last positive day, are the writer's newer
// optional keys: a panel from before they existed leaves them absent.
type mangoTarget struct {
	Target           string           `json:"target"`
	Label            string           `json:"label"`
	Positives        int              `json:"positives"`
	Skipped          bool             `json:"skipped"`
	BestBox          string           `json:"best_box"`
	Conditions       []mangoCondition `json:"conditions"`
	HoldoutPositives *int             `json:"holdout_positives"`
	HoldoutN         int              `json:"holdout_n"`
	HoldoutRate      float64          `json:"holdout_rate"`
	CILow            *float64         `json:"ci_low"`
	CIHigh           *float64         `json:"ci_high"`
	Thin             *bool            `json:"thin"`
	Baseline         float64          `json:"baseline"`
	Verdict          string           `json:"verdict"`
	Reason           string           `json:"reason"`
	LastPositive     string           `json:"last_positive"`
}

// MangoTarget is one target as the page draws it, every string stripped and
// capped, with its optional keys absent when the panel carried none rather than
// zeroed for the page to draw. HoldoutRate and Baseline are fractions of the
// holdout and of the whole set the box was measured against.
type MangoTarget struct {
	Target    string `json:"target"`
	Label     string `json:"label,omitempty"`
	Positives int    `json:"positives"`
	Skipped   bool   `json:"skipped,omitempty"`
	BestBox   string `json:"best_box,omitempty"`
	// Conditions is the box's ranges read alone, in the box's own order, absent
	// when the target has no box.
	Conditions       []MangoCondition `json:"conditions,omitempty"`
	HoldoutPositives *int             `json:"holdout_positives,omitempty"`
	HoldoutN         int              `json:"holdout_n"`
	HoldoutRate      float64          `json:"holdout_rate"`
	CILow            *float64         `json:"ci_low,omitempty"`
	CIHigh           *float64         `json:"ci_high,omitempty"`
	Thin             *bool            `json:"thin,omitempty"`
	Baseline         float64          `json:"baseline"`
	Verdict          string           `json:"verdict"` // earning, no-signal or not-yet
	Reason           string           `json:"reason,omitempty"`
	// LastPositive is the day the target's latest positive landed, absent
	// unless the panel carried a calendar day the reader could parse.
	LastPositive string `json:"last_positive,omitempty"`
}

// MangoCondition is one range of a target's best box as the page draws it: the
// range's text, the share of the recent landings it holds by itself, and
// whether that share is narrow enough to draw as a chip.
type MangoCondition struct {
	Text    string  `json:"text"`
	Share   float64 `json:"share"`
	Narrows bool    `json:"narrows"`
}

// MangoWeek is one week of the panel's trend: the landings that week and the
// share of them rejected before landing.
type MangoWeek struct {
	Week          string  `json:"week"`
	Landings      int     `json:"landings"`
	RejectedShare float64 `json:"rejected_share"`
}

// MangoEvent is one regime change the trend is marked with: the day and what
// changed.
type MangoEvent struct {
	Date string `json:"date"`
	What string `json:"what"`
}

// MangoCounts is how many targets carry each verdict, which is the panel's
// title line.
type MangoCounts struct {
	Earning  int `json:"earning"`
	NoSignal int `json:"no_signal"`
	NotYet   int `json:"not_yet"`
}

// Mango is the town-metrics panel as the page draws it: when the analysis ran,
// whether that is stale, the landings and snapshot hours it covered, and one row
// per target.
type Mango struct {
	At    time.Time `json:"at"`
	State string    `json:"state"` // fresh, stale, missing, error
	// Note names the state in the panel's header: "fresh", "stale", the error
	// that stopped the read, or why there is no panel to read.
	Note string `json:"note,omitempty"`
	// GeneratedAt is when the writer built the panel, absent until one is read.
	GeneratedAt *time.Time `json:"generated_at,omitempty"`
	// Items and SnapshotHours are what the analysis covered: landings and the
	// hours of snapshots joined to them.
	Items         int `json:"items"`
	SnapshotHours int `json:"snapshot_hours"`
	// Weekly is the rejected-share trend, oldest first, and Events the regime
	// changes drawn against it. Both are empty lists, never null, on a panel
	// that carried neither.
	Weekly  []MangoWeek   `json:"weekly"`
	Events  []MangoEvent  `json:"events"`
	Targets []MangoTarget `json:"targets"`
	Counts  MangoCounts   `json:"counts"`
}

// ReadMango returns the town-metrics panel in dir as it stands at now. A panel
// that is not there reads as missing, one the reader cannot use as error, and
// one generated more than mangoStale before now as stale.
func ReadMango(dir string, now time.Time) *Mango {
	m := &Mango{At: now, Targets: []MangoTarget{}, Weekly: []MangoWeek{}, Events: []MangoEvent{}}
	raw, err := readRegularCloudFile(filepath.Join(dir, mangoPanelFile), mangoPanelMaxBytes, errMangoOversize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		m.State, m.Note = MangoMissing, mangoNoPanel
		return m
	case errors.Is(err, errMangoOversize):
		m.State, m.Note = MangoError, mangoOversize
		return m
	case err != nil:
		m.State, m.Note = MangoError, "the panel could not be read"
		return m
	}
	var p mangoPanel
	if err := json.Unmarshal(raw, &p); err != nil {
		m.State, m.Note = MangoError, "the panel is not valid JSON"
		return m
	}
	if p.SchemaVersion != mangoSchema {
		m.State, m.Note = MangoError, fmt.Sprintf("unknown schema version %d", p.SchemaVersion)
		return m
	}
	gen, err := time.Parse(time.RFC3339, strings.TrimSpace(p.GeneratedAt))
	if err != nil {
		m.State, m.Note = MangoError, "the panel has no generated_at timestamp"
		return m
	}
	// The rows are built off to the side so a panel the reader rejects leaves
	// the error state empty: an error that carried part of the file would hand
	// out coverage and counts from a panel it could not use.
	targets, counts, note := mangoTargets(p.Targets)
	if note != "" {
		m.State, m.Note = MangoError, note
		return m
	}
	m.GeneratedAt = &gen
	m.Items, m.SnapshotHours = p.Items, p.SnapshotHours
	m.Weekly, m.Events = mangoWeeks(p.Weekly), mangoEvents(p.Events)
	m.Targets, m.Counts = targets, counts
	m.State, m.Note = mangoFreshness(gen, now)
	return m
}

// mangoFreshness names a panel the reader could use by its generation time: fresh
// until it is mangoStale old, stale after. The hub stamps its snapshots by this
// same rule, so the badge a page draws cannot disagree with the age beside it.
func mangoFreshness(gen, now time.Time) (state, note string) {
	if now.Sub(gen) > mangoStale {
		return MangoStale, "stale"
	}
	return MangoFresh, "fresh"
}

// mangoTargets builds the page's rows and verdict counts from the panel's, each
// string stripped and capped by cloudText, the package's cleaner for another
// process's text (cloud.go). A verdict the page does not know returns its note,
// and the caller reads the panel as an error: an unknown word is not a target to
// draw as any of the three the page knows, and the rows built before it must not
// outlive the rejection. An optional key the reader cannot place is dropped
// instead: it costs the row that one field and leaves the panel readable.
func mangoTargets(in []mangoTarget) ([]MangoTarget, MangoCounts, string) {
	rows := make([]MangoTarget, 0, len(in))
	var counts MangoCounts
	for _, t := range in {
		row := MangoTarget{
			Target:      cloudText(t.Target, mangoTargetMax),
			Label:       cloudText(t.Label, mangoLabelMax),
			Positives:   t.Positives,
			Skipped:     t.Skipped,
			BestBox:     cloudText(t.BestBox, mangoBoxMax),
			HoldoutN:    t.HoldoutN,
			HoldoutRate: t.HoldoutRate,
			Baseline:    t.Baseline,
			Verdict:     strings.ToLower(cloudText(t.Verdict, mangoTargetMax)),
			Reason:      mangoReason(t.Reason),
		}
		row.HoldoutPositives = mangoHoldoutPositives(t)
		row.CILow, row.CIHigh = mangoInterval(t)
		row.Thin = t.Thin
		row.Conditions = mangoConditions(t.Conditions)
		if mangoDay(t.LastPositive) {
			row.LastPositive = t.LastPositive
		}
		switch row.Verdict {
		case MangoEarning:
			counts.Earning++
		case MangoNoSignal:
			counts.NoSignal++
		case MangoNotYet:
			counts.NotYet++
		default:
			return nil, MangoCounts{}, fmt.Sprintf("unknown target verdict %q", row.Verdict)
		}
		rows = append(rows, row)
	}
	return rows, counts, ""
}

// mangoHoldoutPositives is the box's holdout positives when they are a count the
// row could hold: not negative, and no more than the holdout the same row
// reports. A count outside that is not a number this box produced, so the page
// draws no count rather than a contradiction.
func mangoHoldoutPositives(t mangoTarget) *int {
	if t.HoldoutPositives == nil || *t.HoldoutPositives < 0 || *t.HoldoutPositives > t.HoldoutN {
		return nil
	}
	k := *t.HoldoutPositives
	return &k
}

// mangoInterval is the box's Wilson interval when both ends are a fraction and
// the pair is ordered. Both are dropped together: a lone bound is not a range,
// and a reader that kept one would draw an interval the writer never measured.
func mangoInterval(t mangoTarget) (low, high *float64) {
	if t.CILow == nil || t.CIHigh == nil {
		return nil, nil
	}
	lo, hi := *t.CILow, *t.CIHigh
	if !mangoFraction(lo) || !mangoFraction(hi) || lo > hi {
		return nil, nil
	}
	return &lo, &hi
}

// mangoFraction reports whether v is a proportion. NaN fails both comparisons,
// so a value JSON cannot have carried but Go can is dropped too.
func mangoFraction(v float64) bool {
	return v >= 0 && v <= 1
}

// mangoReason is the writer's word for why a target is not-yet, or empty when
// the word is not one the page has a sentence for.
func mangoReason(s string) string {
	switch r := cloudText(s, mangoTargetMax); r {
	case mangoReasonFewPositives, mangoReasonNoHoldoutPositives:
		return r
	}
	return ""
}

// mangoConditions builds a row's chips from the box's conditions: the text is
// stripped and capped, and a share that is not a proportion drops that condition
// alone. The writer's order and narrows flag are kept as written, since the page
// greys the conditions that do not narrow rather than reordering them. The cap
// counts the conditions the page draws, so a dropped one does not shorten the
// list below it.
func mangoConditions(in []mangoCondition) []MangoCondition {
	out := make([]MangoCondition, 0, len(in))
	for _, c := range in {
		if !mangoFraction(c.Share) {
			continue
		}
		out = append(out, MangoCondition{
			Text:    cloudText(c.Text, mangoFreeTextMax),
			Share:   c.Share,
			Narrows: c.Narrows,
		})
		if len(out) == mangoConditionsMax {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mangoWeeks builds the page's trend from the panel's: a row whose week is not a
// calendar day, whose landings are negative or whose share is not a proportion
// is dropped, and the rest keep the writer's oldest-first order. A list over the
// cap keeps its newest weeks, since the newest is the end of the trend the page
// draws.
func mangoWeeks(in []mangoWeek) []MangoWeek {
	out := make([]MangoWeek, 0, len(in))
	for _, w := range in {
		if !mangoDay(w.Week) || w.Landings < 0 || !mangoFraction(w.RejectedShare) {
			continue
		}
		out = append(out, MangoWeek{Week: w.Week, Landings: w.Landings, RejectedShare: w.RejectedShare})
	}
	if len(out) > mangoWeeklyMax {
		out = out[len(out)-mangoWeeklyMax:]
	}
	return out
}

// mangoEvents builds the page's markers from the panel's: a row whose date is
// not a calendar day is dropped, and what is stripped and capped like every
// other string. A list over the cap keeps its newest events, the same end of the
// list the trend's cap keeps.
func mangoEvents(in []mangoEvent) []MangoEvent {
	out := make([]MangoEvent, 0, len(in))
	for _, e := range in {
		if !mangoDay(e.Date) {
			continue
		}
		out = append(out, MangoEvent{Date: e.Date, What: cloudText(e.What, mangoFreeTextMax)})
	}
	if len(out) > mangoEventsMax {
		out = out[len(out)-mangoEventsMax:]
	}
	return out
}

// mangoDay reports whether s is a calendar day in the writer's date format. The
// layout has no time of day, so a timestamp is not a day this page can place.
func mangoDay(s string) bool {
	_, err := time.Parse(mangoDayLayout, s)
	return err == nil
}
