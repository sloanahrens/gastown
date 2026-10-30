// Package health judges whether each Jev battery is still collecting data.
//
// The rules are pure functions over what the caller read: a battery's log
// lines, its cursor file's modification time, the patrol config, the usage
// ledger's calls by caller and the clock. Every status is a unit test over
// fixtures. The command that reads the files and prints the rows is
// gt jev health (internal/cmd/jev_health.go), which calls Report.
package health

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/jev/attention"
	"github.com/steveyegge/gastown/internal/jev/conflict"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/jev/drift"
	"github.com/steveyegge/gastown/internal/jev/failcause"
	"github.com/steveyegge/gastown/internal/jev/landing"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
	"github.com/steveyegge/gastown/internal/jev/notes"
	"github.com/steveyegge/gastown/internal/jev/progress"
	"github.com/steveyegge/gastown/internal/jev/rework"
	"github.com/steveyegge/gastown/internal/jev/rules"
	"github.com/steveyegge/gastown/internal/jev/stale"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/jev/targets"
	"github.com/steveyegge/gastown/internal/jev/testsel"
	"github.com/steveyegge/gastown/internal/jev/thrash"
)

// Cadence is how often a battery is expected to write a line.
type Cadence string

const (
	// QuarterHour is a daemon patrol that runs every fifteen minutes.
	QuarterHour Cadence = "15m"
	// Weekly is a battery the daemon runs once a week.
	Weekly Cadence = "weekly"
	// OnLanding is a battery only a landing's gate writes, so a day without a
	// landing leaves it quiet by design.
	OnLanding Cadence = "landing"
	// OnEvent is a battery that writes only when an event it watches happens.
	OnEvent Cadence = "event"
)

// Status is one battery's verdict.
type Status string

const (
	// OK means the battery is enabled and collecting on its cadence.
	OK Status = "ok"
	// Disabled means the patrol config turns the battery off.
	Disabled Status = "disabled"
	// Stale means a continuous battery's last pass is over StaleAfter old.
	Stale Status = "stale"
	// Late means a weekly battery's last pass is over LateAfter old.
	Late Status = "late"
	// Idle means the battery writes only on an event, so silence is expected.
	Idle Status = "idle by design"
	// Low means the battery wrote under LowShare of its 7-day median.
	Low Status = "low"
	// Unanswered means over UnansweredShare of its last day's lines carry no
	// answers.
	Unanswered Status = "unanswered"
	// Unparseable means the battery's log holds lines and none of them parses:
	// no count can say what it collected, and reading that as quiet would hide
	// a schema change behind an empty row.
	Unparseable Status = "unparseable"
	// Drifted means the battery's log parses, but more than DriftShare of its
	// lines do not: part of what it collects can no longer be read, and
	// counting only the readable part would pass a half-broken log as ok.
	Drifted Status = "drifted"
	// Unknown means the evidence a rule needs cannot be read: a battery only a
	// landing writes, judged against a landing clock the town has not recorded.
	Unknown Status = "unknown"
)

// Fails reports whether the status means an enabled battery is not collecting.
func (s Status) Fails() bool {
	switch s {
	case Stale, Late, Low, Unanswered, Unparseable, Drifted, Unknown:
		return true
	}
	return false
}

const (
	// Window is the period the last-24-hours counts cover.
	Window = 24 * time.Hour
	// BaselineDays is the number of daily buckets the median is taken over.
	BaselineDays = 7
	// StaleAfter is how far behind a continuous battery may fall before it is
	// stale: three missed fifteen-minute passes.
	StaleAfter = 45 * time.Minute
	// LateAfter is how far behind a weekly battery may fall: a day past its
	// week.
	LateAfter = 8 * 24 * time.Hour
	// LowMedian is the baseline median below which a light count says nothing:
	// a battery that wrote three lines a day is allowed to fall quiet.
	LowMedian = 20
	// LowShare is the fraction of the baseline median a last-day count must
	// reach.
	LowShare = 0.25
	// DriftShare is the fraction of a log's lines that may fail to parse
	// before the battery reads drifted: a few stray lines from an older
	// format are normal, a tenth or more of the log is a schema change.
	DriftShare = 0.1
	// UnansweredShare is the fraction of answer-free lines above which a
	// battery is writing lines without collecting answers.
	UnansweredShare = 0.5
)

// Battery is one battery's identity and expected cadence.
type Battery struct {
	// Name is the battery's name, the row's first column.
	Name string
	// ConfigKey is the daemon.patrols key that turns the battery on.
	ConfigKey string
	// Cadence is how often the battery is expected to write.
	Cadence Cadence
	// Caller is the name the battery spends under in the usage ledger.
	Caller string
	// LogPath is the battery's own log file function, so the reader opens the
	// file the battery writes.
	LogPath func(townRoot string) string
	// CursorPath is the battery's own cursor file function, nil for a battery
	// that keeps no cursor.
	CursorPath func(townRoot string) string
	// IdleReason is why a battery that writes only on an event is idle by
	// design, "" for a battery expected on a schedule.
	IdleReason string
	// DeepReviewIdle marks the battery only the deep-review schedule writes:
	// while scheduled_slings is off nothing calls it.
	DeepReviewIdle bool
}

// Batteries is every battery health judges, in name order.
var Batteries = []Battery{
	{
		Name: "attention", ConfigKey: "jev_attention", Cadence: QuarterHour,
		Caller: attention.Caller, LogPath: attention.LogPath, CursorPath: attention.CursorPath,
	},
	{
		Name: "conflict", ConfigKey: "jev_conflict", Cadence: QuarterHour,
		Caller: conflict.Caller, LogPath: conflict.LogPath, CursorPath: conflict.CursorPath,
	},
	{
		Name: "dedupe", ConfigKey: "jev_dedupe", Cadence: QuarterHour,
		Caller: dedupe.Caller, LogPath: dedupe.LogPath, CursorPath: dedupe.CursorPath,
	},
	{
		Name: "dispatch", ConfigKey: "jev_dispatch", Cadence: QuarterHour,
		Caller: dispatch.Caller, LogPath: dispatch.LogPath, CursorPath: dispatch.CursorPath,
	},
	{
		Name: "drift", ConfigKey: "jev_drift", Cadence: QuarterHour,
		Caller: drift.Caller, LogPath: drift.LogPath, CursorPath: drift.CursorPath,
	},
	{
		Name: "failcause", ConfigKey: "jev_failcause", Cadence: OnEvent,
		Caller: failcause.Caller, LogPath: failcause.LogPath,
		IdleReason: "only a gate that fails a Go test writes this",
	},
	{
		Name: "landing", ConfigKey: "jev_landing", Cadence: QuarterHour,
		Caller: landing.Caller, LogPath: landing.LogPath, CursorPath: landing.CursorPath,
	},
	{
		Name: "logtriage", ConfigKey: "jev_logtriage", Cadence: QuarterHour,
		Caller: logtriage.Caller, LogPath: logtriage.LogPath, CursorPath: logtriage.CursorPath,
	},
	{
		Name: "notes", ConfigKey: "jev_notes", Cadence: Weekly,
		Caller: notes.Caller, LogPath: notes.LogPath,
	},
	{
		Name: "progress", ConfigKey: "jev_progress", Cadence: OnEvent,
		Caller: progress.Caller, LogPath: progress.LogPath,
		IdleReason: "only the overrun guard nudging a polecat writes this",
	},
	{
		Name: "rework", ConfigKey: "jev_rework", Cadence: QuarterHour,
		Caller: rework.Caller, LogPath: rework.LogPath, CursorPath: rework.CursorPath,
	},
	{
		Name: "rules", ConfigKey: "jev_rules", Cadence: QuarterHour,
		Caller: rules.Caller, LogPath: rules.LogPath, CursorPath: rules.CursorPath,
	},
	{
		Name: "stale", ConfigKey: "jev_stale", Cadence: Weekly,
		Caller: stale.Caller, LogPath: stale.LogPath,
	},
	{
		Name: "submission", ConfigKey: "jev_submission", Cadence: QuarterHour,
		Caller: submission.Caller, LogPath: submission.LogPath, CursorPath: submission.CursorPath,
	},
	{
		Name: "targets", ConfigKey: "jev_targets", Cadence: OnEvent,
		Caller: targets.Caller, LogPath: targets.LogPath, DeepReviewIdle: true,
	},
	{
		Name: "testsel", ConfigKey: "jev_testsel", Cadence: OnLanding,
		Caller: testsel.Caller, LogPath: testsel.LogPath,
	},
	{
		Name: "thrash", ConfigKey: "jev_thrash", Cadence: QuarterHour,
		Caller: thrash.Caller, LogPath: thrash.LogPath, CursorPath: thrash.CursorPath,
	},
}

// Config is the patrol configuration health reads.
type Config struct {
	// Enabled maps a daemon.patrols key to whether the town turned it on.
	Enabled map[string]bool
	// ScheduledSlings is daemon.patrols.scheduled_slings.enabled.
	ScheduledSlings bool
}

// Input is the state the caller read for one battery.
type Input struct {
	// Cursor is the cursor file's modification time, and HasCursor is false
	// when the battery keeps no cursor or has not written one yet.
	Cursor    time.Time
	HasCursor bool
	// Log is every line of the battery's log that parsed.
	Log []Line
	// Unparsed is how many of the log's non-blank lines did not parse: a line
	// a schema change broke must be counted, not read as a quiet battery.
	Unparsed int
	// LogPresent is whether the log file exists at all. An absent log is a
	// battery that has never written; a log that exists with no readable line
	// is a schema change, and only this tells them apart.
	LogPresent bool
}

// landingClockKnown is whether the landing battery's log can say when the town
// last landed: one line per landing, and no readable line (or no file at all)
// leaves a battery only a landing writes with no day to be quiet against.
func landingClockKnown(in Input, now time.Time) bool {
	if !in.LogPresent {
		return false
	}
	_, ok := newestLine(in.Log, now)
	return ok
}

// Line is the part of one log line health reads.
type Line struct {
	// TS is when the line was logged.
	TS time.Time
	// HasAnswers is whether the line carries any answer.
	HasAnswers bool
}

// Row is one battery's verdict and the counts behind it.
type Row struct {
	Name    string  `json:"name"`
	Enabled bool    `json:"enabled"`
	Cadence Cadence `json:"cadence"`
	// LastPass is nil when no pass is recorded.
	LastPass      *time.Time `json:"last_pass"`
	Lines24       int        `json:"lines_24h"`
	Median7       int        `json:"median_7d"`
	LedgerCalls   int        `json:"ledger_calls_24h"`
	NoAnswerShare float64    `json:"no_answer_share"`
	Unparsed      int        `json:"unparsed_lines"`
	Status        Status     `json:"status"`
	Reason        string     `json:"reason,omitempty"`
}

// landingsBattery is the battery whose log records each landing the town
// reviewed; its last day is the landing clock an OnLanding battery is judged
// against.
const landingsBattery = "landing"

// Report judges every battery in Batteries, in table order, from each
// battery's files in inputs and each caller's calls in the last day in ledger.
func Report(cfg Config, inputs map[string]Input, ledger map[string]int, now time.Time) []Row {
	landingInput := inputs[landingsBattery]
	landings24 := countBetween(landingInput.Log, now.Add(-Window), now)
	landingKnown := landingClockKnown(landingInput, now)
	rows := make([]Row, 0, len(Batteries))
	for _, b := range Batteries {
		rows = append(rows, judge(b, cfg, inputs[b.Name], ledger, landingKnown, landings24, now))
	}
	return rows
}

// ExitFails reports whether any row fails, which the command turns into exit
// status 1.
func ExitFails(rows []Row) bool {
	for _, r := range rows {
		if r.Status.Fails() {
			return true
		}
	}
	return false
}

// judge builds one battery's row.
func judge(b Battery, cfg Config, in Input, ledger map[string]int, landingKnown bool, landings24 int, now time.Time) Row {
	row := Row{
		Name:          b.Name,
		Enabled:       cfg.Enabled[b.ConfigKey],
		Cadence:       b.Cadence,
		Lines24:       countBetween(in.Log, now.Add(-Window), now),
		Median7:       medianDaily(in.Log, now),
		LedgerCalls:   ledger[b.Caller],
		NoAnswerShare: noAnswerShare(in.Log, now),
		Unparsed:      in.Unparsed,
	}
	if t, ok := lastPass(in, now); ok {
		row.LastPass = &t
	}
	row.judgeStatus(b, cfg, in, landingKnown, landings24, now)
	if row.Status == OK && unparsedShare(in) > DriftShare {
		row.Status = Drifted
		row.Reason = fmt.Sprintf("%d of %d log lines did not parse", in.Unparsed, in.Unparsed+len(in.Log))
	} else if row.Unparsed > 0 && row.Status != Unparseable {
		row.Reason = reasonWith(row.Reason, fmt.Sprintf("%d log lines did not parse", row.Unparsed))
	}
	return row
}

// unparsedShare is the fraction of the log's non-blank lines that did not
// parse, zero for an empty log.
func unparsedShare(in Input) float64 {
	total := in.Unparsed + len(in.Log)
	if total == 0 {
		return 0
	}
	return float64(in.Unparsed) / float64(total)
}

// reasonWith joins a verdict's reason with a note about the row, "; " between.
func reasonWith(reason, note string) string {
	if reason == "" {
		return note
	}
	return reason + "; " + note
}

// judgeStatus sets the first rule the row breaks, reading the config and the
// freshness of the last pass before the counts: a stale battery also logs
// nothing, and naming the missed pass says more than naming the empty count.
func (r *Row) judgeStatus(b Battery, cfg Config, in Input, landingKnown bool, landings24 int, now time.Time) {
	switch {
	case !r.Enabled:
		r.Status, r.Reason = Disabled, "patrols."+b.ConfigKey+" is off"
		return
	case b.DeepReviewIdle && !cfg.ScheduledSlings:
		r.Status = Idle
		r.Reason = "only the deep-review schedule writes this, and scheduled_slings is off"
		return
	case b.IdleReason != "":
		r.Status, r.Reason = Idle, b.IdleReason
		return
	}

	// A log that holds lines and parses none of them says nothing about the
	// battery, so no freshness or count rule below can be read from it.
	if in.LogPresent && len(in.Log) == 0 && in.Unparsed > 0 {
		r.Status, r.Reason = Unparseable, "none of the log's lines parse"
		return
	}

	if b.Cadence == OnLanding {
		r.judgeLanding(landingKnown, landings24)
		return
	}

	if r.LastPass == nil {
		if b.Cadence == Weekly || b.Cadence == OnEvent {
			r.Status = Late
		} else {
			r.Status = Stale
		}
		r.Reason = "no pass recorded"
		return
	}
	age := now.Sub(*r.LastPass)
	// A battery with no schedule (one that only an event writes and is not
	// idle by design) is bounded only by the weekly limit: the fifteen-minute
	// limit would call it stale between the events it waits for.
	if b.Cadence == Weekly || b.Cadence == OnEvent {
		if age > LateAfter {
			r.Status, r.Reason = Late, "last pass "+ageText(age)+" ago"
			return
		}
	} else if age > StaleAfter {
		r.Status, r.Reason = Stale, "last pass "+ageText(age)+" ago"
		return
	}

	r.judgeCounts()
}

// judgeLanding judges a battery only a landing writes by whether the town kept
// landing while the battery stopped logging: a day with landings and no line
// here is a gate that stopped calling it, and a day with no landing is quiet.
// Without a landing clock there is no day to be quiet against, so the row says
// so rather than passing as ok.
func (r *Row) judgeLanding(landingKnown bool, landings24 int) {
	switch {
	case !landingKnown:
		r.Status = Unknown
		r.Reason = "no landing clock: the landing battery has logged no landing"
	case landings24 == 0:
		r.Status, r.Reason = OK, "no landing in the last day"
	case r.Lines24 == 0:
		r.Status = Stale
		r.Reason = fmt.Sprintf("%d landings in the last day, none logged here", landings24)
	default:
		r.judgeCounts()
	}
}

// judgeCounts sets low or unanswered from the row's counts, ok otherwise.
func (r *Row) judgeCounts() {
	if r.Median7 >= LowMedian && float64(r.Lines24) < float64(r.Median7)*LowShare {
		r.Status = Low
		r.Reason = fmt.Sprintf("%d lines in 24h against a median of %d", r.Lines24, r.Median7)
		return
	}
	if r.Lines24 > 0 && r.NoAnswerShare > UnansweredShare {
		r.Status = Unanswered
		r.Reason = fmt.Sprintf("%.0f%% of the last 24h's lines carry no answers", r.NoAnswerShare*100)
		return
	}
	r.Status = OK
}

// lastPass is the time the battery last ran: its cursor file's modification
// time, or the newest line of a battery that keeps no cursor or has none yet.
func lastPass(in Input, now time.Time) (time.Time, bool) {
	if in.HasCursor {
		return in.Cursor, true
	}
	return newestLine(in.Log, now)
}

// newestLine is the newest line stamped before now. A line stamped in the
// future names no pass that has happened, and the window excludes it too, so
// the last pass cannot be read from it.
func newestLine(lines []Line, now time.Time) (time.Time, bool) {
	var last time.Time
	for _, l := range lines {
		if !l.TS.Before(now) {
			continue
		}
		if l.TS.After(last) {
			last = l.TS
		}
	}
	return last, !last.IsZero()
}

// countBetween counts the lines stamped in [from, to).
func countBetween(lines []Line, from, to time.Time) int {
	n := 0
	for _, l := range lines {
		if !l.TS.Before(from) && l.TS.Before(to) {
			n++
		}
	}
	return n
}

// medianDaily is the median of the line counts of the seven days ending a day
// before now. The bucket bounds are half-open, so a line on a boundary counts
// for one day only.
func medianDaily(lines []Line, now time.Time) int {
	counts := make([]int, BaselineDays)
	for i := range counts {
		to := now.Add(-time.Duration(i+1) * Window)
		counts[i] = countBetween(lines, to.Add(-Window), to)
	}
	sort.Ints(counts)
	return counts[len(counts)/2]
}

// noAnswerShare is the fraction of the last day's lines that carry no answer.
func noAnswerShare(lines []Line, now time.Time) float64 {
	from := now.Add(-Window)
	total, unanswered := 0, 0
	for _, l := range lines {
		if l.TS.Before(from) || !l.TS.Before(now) {
			continue
		}
		total++
		if !l.HasAnswers {
			unanswered++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(unanswered) / float64(total)
}

// ageText renders how long ago a pass was, coarsely enough to read in a table.
func ageText(d time.Duration) string {
	if d < 0 {
		return "just now"
	}
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}

// ParseLog decodes a battery log's JSON lines into the parts health reads,
// returning how many non-blank lines it could not read: a line that does not
// parse or carries no stamp names no pass any window can count, and the count
// is what keeps such a line from reading as a quiet battery.
func ParseLog(data []byte) (lines []Line, unparsed int) {
	for _, raw := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var rec struct {
			TS      time.Time       `json:"ts"`
			Answers json.RawMessage `json:"answers"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil || rec.TS.IsZero() {
			unparsed++
			continue
		}
		lines = append(lines, Line{TS: rec.TS, HasAnswers: carriesAnswers(rec.Answers)})
	}
	return lines, unparsed
}

// carriesAnswers reports whether an answers field holds any answer. Every
// battery writes answers as an object, or null when no answer came back.
func carriesAnswers(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var answers map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &answers); err != nil {
		return false
	}
	return len(answers) > 0
}
