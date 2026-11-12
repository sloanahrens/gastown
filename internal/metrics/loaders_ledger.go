package metrics

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/land"
)

// This file holds the ledger and review loaders. Each takes its source by
// injection, records every refused line in the Dropped log, and returns the rows
// it could attribute, so rows plus drops accounts for every non-blank line.

// jsonRows returns the JSON of each non-blank line, in order. A line that is
// not JSON is recorded in dropped and yields nil, so a caller sees one entry
// per non-blank line and never mistakes a drop for a record.
func jsonRows(lines []string, source string, dropped *Dropped) []json.RawMessage {
	rows := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			dropped.Add(source, "unparseable json")
			rows = append(rows, nil)
			continue
		}
		rows = append(rows, json.RawMessage(line))
	}
	return rows
}

// LedgerRow is one line of a rig's ledger.jsonl: the fields the loader keeps,
// with Rig filled from the source rig when the line names none. Branch is kept
// because it names the seat that wrote the work that landed.
//
// PolecatSecs, PolecatLowerBound and Agent are the polecat time and seat the
// landing worker writes since 10-26. The writer omits a zero time and a false
// bound, so a line that carries none of them reads as zero here, which the
// landings table reads as no time.
//
// PolecatAttempts is one entry per seat's time on the bead, as the landing
// record writes it. The table reads only its length.
type LedgerRow struct {
	Bead              string            `json:"bead"`
	Rig               string            `json:"rig"`
	LandedCommit      string            `json:"landed_commit"`
	LandedAt          string            `json:"landed_at"`
	Route             string            `json:"route"`
	OMVerdict         string            `json:"om_verdict"`
	GateResult        string            `json:"gate_result"`
	Branch            string            `json:"branch"`
	PolecatSecs       float64           `json:"polecat_secs"`
	PolecatLowerBound bool              `json:"polecat_lower_bound"`
	Agent             string            `json:"agent"`
	PolecatAttempts   []json.RawMessage `json:"polecat_attempts"`
}

// LoadLedger parses each rig's ledger lines into rows. A line that is not JSON,
// or that names no bead or no landed_at, is recorded in dropped instead. A line
// that names no rig is attributed to the source rig.
//
// Rigs are visited in sorted order, since a Go map keeps no insertion order;
// sorting keeps the row order stable from run to run.
func LoadLedger(sources map[string][]string, dropped *Dropped) []LedgerRow {
	rigs := make([]string, 0, len(sources))
	for rig := range sources {
		rigs = append(rigs, rig)
	}
	sort.Strings(rigs)
	var rows []LedgerRow
	for _, rig := range rigs {
		source := "ledger:" + rig
		for _, raw := range jsonRows(sources[rig], source, dropped) {
			if raw == nil {
				continue
			}
			var row LedgerRow
			// Valid JSON that is not an object has no bead and no landed_at;
			// it takes the same refusal, so the loader records it
			// rather than stopping the load.
			if err := json.Unmarshal(raw, &row); err != nil || row.Bead == "" || row.LandedAt == "" {
				dropped.Add(source, "missing bead or landed_at")
				continue
			}
			if row.Rig == "" {
				row.Rig = rig
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// StageStamps is the per-bead stage data the landings join reads besides the
// ledger, the beads and the usage summary: the event feed's dispatch and done
// clocks, and the seats the usage records name. A bead with no entry keeps its
// landing's column empty and is counted in the footer, never dropped.
type StageStamps struct {
	// Dispatched is each bead's run dispatch in RFC3339: when the run that
	// produced its landing went out (events.DispatchedBy).
	Dispatched map[string]string
	// Submitted is each bead's latest done event that submitted work, in
	// RFC3339, the fallback for a bead whose notes carry no submission stamp.
	// An escalated or deferred exit writes a done event too, and stamps
	// nothing here (gt-qqmae).
	Submitted map[string]string
	// Seats is who the usage records say ran each bead and on what model.
	Seats UsageSeats
}

// LoadStageStamps folds the events feed's lines once for every landed bead in
// landedAt. A line that is not an event is recorded in dropped rather than read
// as a dispatch or a submission.
//
// A dispatch is the sling of the run that ended in the bead's last submission
// before it landed: the feed's slings and run ends before that submission,
// folded in order (the same clock the landing worker times a bead from,
// gt-v7ptp). The landed run is read through its submission because a done event
// ends its run; read at landed_at instead, the clock would be the empty run the
// done left behind. A bead the feed holds no such sling for has no dispatch, so
// the cell stays empty rather than borrow a later run's.
func LoadStageStamps(lines []string, landedAt map[string]string, dropped *Dropped) StageStamps {
	stamps := StageStamps{
		Dispatched: make(map[string]string, len(landedAt)),
		Submitted:  make(map[string]string),
		Seats: UsageSeats{
			SeatModels: map[string]string{},
			BeadSeats:  map[string]string{},
		},
	}
	byBead := make(map[string][]events.Event)
	submitted := make(map[string]time.Time)
	done := make(map[string][]time.Time)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev events.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Type == "" {
			dropped.Add("events", "unparseable event")
			continue
		}
		bead, _ := ev.Payload["bead"].(string)
		if bead == "" {
			continue
		}
		byBead[bead] = append(byBead[bead], ev)
		if ev.Type != events.TypeDone {
			continue
		}
		at, err := doneEventTime(ev)
		if err != nil {
			continue
		}
		// Every done event ends the bead's run, so done feeds the landing
		// clock; only a submission stamps Submitted (gt-qqmae).
		done[bead] = append(done[bead], at)
		if events.DoneEventIsSubmission(ev) && at.After(submitted[bead]) {
			submitted[bead] = at
		}
	}
	for bead, at := range submitted {
		stamps.Submitted[bead] = at.UTC().Format(time.RFC3339)
	}
	for bead, landed := range landedAt {
		end, err := ParseTS(landed)
		if err != nil {
			// BuildLandings refuses the unparsable landing; no stamp is read
			// for it here, so this pass never guesses one.
			continue
		}
		// The run that landed is the one the last submission before the landing
		// ended, not the run open at the landing: a done event ends its run, so
		// the clock read at landed_at would be the empty run after it.
		var lastDone time.Time
		for _, at := range done[bead] {
			if !at.After(end) && at.After(lastDone) {
				lastDone = at
			}
		}
		if !lastDone.IsZero() {
			end = lastDone
		}
		at, _, ok := events.DispatchedBy(byBead[bead], bead, end)
		if !ok || at.IsZero() {
			continue
		}
		stamps.Dispatched[bead] = at.UTC().Format(time.RFC3339)
	}
	return stamps
}

// doneEventTime is when a done event's submission happened: the "at" its writer
// recorded, or the event's own timestamp when it recorded none. A done event a
// sandbox seat could not write is re-recorded by the host with the submission's
// own time, so the "at" is the seat's turn ending and the timestamp is only
// when it reached this log (gt-1m9gr).
func doneEventTime(ev events.Event) (time.Time, error) {
	if at, ok := ev.Payload["at"].(string); ok && at != "" {
		if ts, err := time.Parse(time.RFC3339, at); err == nil {
			return ts, nil
		}
	}
	return time.Parse(time.RFC3339, ev.Timestamp)
}

// Review is one line of reviews.jsonl: the om verdict that judged a tree.
// Attempt is the landing attempt the row carries, so a reader's round is that
// field and never a recount of the bead's reviews.
type Review struct {
	Bead    string
	Rig     string
	Attempt int
	Score   float64
	Verdict string
	At      string
	// Author is the seat that wrote the branch the review judged, and
	// AuthorKind its kind; AttributeReviews fills both, and they are empty
	// until it does.
	Author     string
	AuthorKind string
}

// reviewFields is a review row mid-parse: a pointer per required field tells an
// absent field from a zero value.
type reviewFields struct {
	Bead    *string  `json:"bead"`
	Rig     *string  `json:"rig"`
	Attempt *int     `json:"attempt"`
	Score   *float64 `json:"score"`
	Verdict *string  `json:"verdict"`
	At      *string  `json:"at"`
}

// LoadReviews parses the review log's lines into rows. A line that is not JSON,
// or that lacks a field a review needs, is recorded in dropped: a bad row is
// never read as a zero-score review. A follow-up queue line is not a verdict at
// all but the feed's own bookkeeping, so land.IsQueueLine decides it and the
// row is skipped uncounted (gt-3arxj).
func LoadReviews(lines []string, dropped *Dropped) []Review {
	var rows []Review
	for _, raw := range jsonRows(lines, "reviews", dropped) {
		if raw == nil || land.IsQueueLine(raw) {
			continue
		}
		var line reviewFields
		if err := json.Unmarshal(raw, &line); err != nil ||
			line.Bead == nil || line.Attempt == nil || line.Score == nil ||
			line.Verdict == nil || line.At == nil {
			dropped.Add("reviews", "missing field")
			continue
		}
		row := Review{
			Bead:    *line.Bead,
			Attempt: *line.Attempt,
			Score:   *line.Score,
			Verdict: *line.Verdict,
			At:      *line.At,
		}
		if line.Rig != nil {
			row.Rig = *line.Rig
		}
		rows = append(rows, row)
	}
	return rows
}

// OMRoundsByBead counts each landed bead's om verdicts at or before its landing.
// Every review in the list is a verdict: LoadReviews has already set the
// follow-up queue lines aside. A verdict whose timestamp does not parse is
// skipped with one logged line, so one bad line does not stop the table for the
// other landings. A landing time that does not parse is an error, raised when a
// verdict is placed against that landing. A bead with no verdict at or before its
// landing has no entry.
func OMRoundsByBead(reviews []Review, landedAt map[string]string, dropped *Dropped) (map[string]int, error) {
	rounds := make(map[string]int, len(landedAt))
	for _, r := range reviews {
		landed, ok := landedAt[r.Bead]
		if !ok {
			continue
		}
		landedTime, err := ParseTS(landed)
		if err != nil {
			return nil, fmt.Errorf("landed_at of %s %q: %w", r.Bead, landed, err)
		}
		at, err := ParseTS(r.At)
		if err != nil {
			dropped.Add("reviews", fmt.Sprintf("om verdict of %s skipped: unparseable at %q", r.Bead, r.At))
			continue
		}
		if !at.After(landedTime) {
			rounds[r.Bead]++
		}
	}
	return rounds, nil
}

// Rework is one landed bead's rework history.
type Rework struct {
	// Attempts is the larger of the review log's attempt count and the
	// rejection notes' count plus one.
	Attempts int
	// RejectedBeforeLanding says a rejection predates the bead's landing.
	RejectedBeforeLanding bool
}

// ReworkByBead returns attempts and rejected-before-landing per landed bead.
//
// reviews.jsonl only starts mid-October; the MERGE REJECTION notes on the bead
// reach back to the first landing, so both count: a bead is rejected if either
// source says so, and attempts is the larger of the two views. A review whose
// bead never landed is ignored.
//
// A timestamp that does not parse is an error:
// a malformed date would otherwise compare as zero and undercount.
func ReworkByBead(reviews []Review, landedAt map[string]string, noteKinds map[string][]string) (map[string]Rework, error) {
	out := make(map[string]Rework, len(landedAt))
	for bead := range landedAt {
		kinds := noteKinds[bead]
		out[bead] = Rework{Attempts: 1 + len(kinds), RejectedBeforeLanding: len(kinds) > 0}
	}
	for _, r := range reviews {
		cur, landed := out[r.Bead]
		if !landed {
			continue
		}
		cur.Attempts = max(cur.Attempts, r.Attempt)
		if r.Verdict == "request_changes" {
			at, err := ParseTS(r.At)
			if err != nil {
				return nil, fmt.Errorf("review of %s at %q: %w", r.Bead, r.At, err)
			}
			landedTime, err := ParseTS(landedAt[r.Bead])
			if err != nil {
				return nil, fmt.Errorf("landed_at of %s %q: %w", r.Bead, landedAt[r.Bead], err)
			}
			if !at.After(landedTime) {
				cur.RejectedBeforeLanding = true
			}
		}
		out[r.Bead] = cur
	}
	return out, nil
}
