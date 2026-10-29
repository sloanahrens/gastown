package metrics

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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

// LedgerRow is one line of a rig's ledger.jsonl: the seven fields the
// loader keeps, with Rig filled from the source rig when the line names none.
type LedgerRow struct {
	Bead         string `json:"bead"`
	Rig          string `json:"rig"`
	LandedCommit string `json:"landed_commit"`
	LandedAt     string `json:"landed_at"`
	Route        string `json:"route"`
	OMVerdict    string `json:"om_verdict"`
	GateResult   string `json:"gate_result"`
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
