package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// storyPointsAdoptionWindow is how far back the adoption items reach: an
// adoption or a revert the weekly run made in the last seven days is raised, and
// an older one is not (gt-g2dgme).
const storyPointsAdoptionWindow = 7 * 24 * time.Hour

// storyPointsUnreadableKey is the item raised when the adoptions log cannot be
// read or does not parse. A town whose log is unreadable would otherwise look
// exactly like a town that never adopted anything (gt-g2dgme).
const storyPointsUnreadableKey = "story-points:unreadable"

// storyPointsRefusedKey is the item raised when the weekly run refused the
// proposal two weeks in a row: the weights have not moved and nobody has been
// told why (gt-ti2sdg).
const storyPointsRefusedKey = "story-points:refused"

// storyPointsAdoptionLine is one line of the adoptions log the weekly run writes
// beside the runtime weights (internal/metrics/storypoints_adopt.go). An
// "adopted" line names the version the weights were kept as. A "checked" line is
// the week-after judgement of the adoption at AdoptedAt: "kept" or "reverted".
// A "refused" line carries the rule's Reason.
type storyPointsAdoptionLine struct {
	Event     string     `json:"event"`
	At        time.Time  `json:"at"`
	Version   string     `json:"version,omitempty"`
	AdoptedAt *time.Time `json:"adopted_at,omitempty"`
	Outcome   string     `json:"outcome,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

// storyPointsAdoptionLogPath is the adoptions log under the town, the file the
// metrics writer appends to (storyPointsAdoptionsPath in internal/metrics).
func storyPointsAdoptionLogPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "storypoints", "adoptions.jsonl")
}

// readStoryPointsAdoptionLog reads every line of the adoptions log in order. A
// missing file is no adoption yet and reads as no lines. Any other read error,
// and any line that does not parse, is an error: the log is not read as a
// shorter log, since a truncated read would hide the adoption it dropped.
func readStoryPointsAdoptionLog(path string) ([]storyPointsAdoptionLine, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var lines []storyPointsAdoptionLine
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var l storyPointsAdoptionLine
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// collectStoryPointsAdoptions raises one info item per adoption and per revert
// the weekly run made in the last seven days, and one unreadable item when the
// adoptions log cannot be read. It never returns an error: a failed read is the
// unreadable item, so the queue never reads a broken log as no adoptions.
//
// A hand-built sources value with no log read raises nothing, the same as the
// drift collector, so a test fixture that does not carry the read is unchanged.
func (s *attentionSources) collectStoryPointsAdoptions(context.Context) ([]attention.Item, error) {
	if s.storyPointsAdoptions == nil {
		return nil, nil
	}
	lines, err := s.storyPointsAdoptions()
	if err != nil {
		return []attention.Item{{
			Key:      storyPointsUnreadableKey,
			Kind:     attention.KindStoryPoints,
			Severity: attention.SeverityLow,
			Summary:  "story points adoptions cannot be read: " + err.Error(),
		}}, nil
	}
	return storyPointsAdoptionItems(lines, s.now), nil
}

// storyPointsAdoptionItems is the adopted and reverted items for the lines
// recorded in the window ending at now. A revert names the version of the
// adoption it judged, found by its adopted_at time; a revert whose adoption is
// not in the log names that time instead. The last two weekly outcomes both
// refused raise the refused item (gt-ti2sdg).
func storyPointsAdoptionItems(lines []storyPointsAdoptionLine, now time.Time) []attention.Item {
	since := now.Add(-storyPointsAdoptionWindow)
	versions := map[string]string{}
	for _, l := range lines {
		if l.Event == "adopted" {
			versions[storyPointsStamp(l.At)] = l.Version
		}
	}
	var out []attention.Item
	for _, l := range lines {
		if l.At.Before(since) {
			continue
		}
		switch {
		case l.Event == "adopted":
			out = append(out, storyPointsAttentionItem(
				"story-points:adopted:"+storyPointsStamp(l.At),
				fmt.Sprintf("story points weights adopted: version %s on %s by the weekly run; the week-after check judges it",
					l.Version, storyPointsDate(l.At))))
		case l.Event == "checked" && l.Outcome == "reverted":
			version := "the adoption at " + storyPointsStamp(l.At)
			if l.AdoptedAt != nil {
				adopted := storyPointsStamp(*l.AdoptedAt)
				version = "the adoption at " + adopted
				if v, ok := versions[adopted]; ok {
					version = "version " + v
				}
			}
			out = append(out, storyPointsAttentionItem(
				"story-points:reverted:"+storyPointsStamp(l.At),
				fmt.Sprintf("story points weights reverted: %s was reverted on %s by the week-after check, which found it did worse",
					version, storyPointsDate(l.At))))
		}
	}
	// The last two weekly outcomes, adopted or refused lines in append-only file
	// order, both refused: a "checked" line between them is not an outcome.
	var outcomes []storyPointsAdoptionLine
	for _, l := range lines {
		if l.Event == "adopted" || l.Event == "refused" {
			outcomes = append(outcomes, l)
			if len(outcomes) > 2 {
				outcomes = outcomes[1:]
			}
		}
	}
	if len(outcomes) == 2 && outcomes[0].Event == "refused" && outcomes[1].Event == "refused" {
		out = append(out, storyPointsAttentionItem(storyPointsRefusedKey, fmt.Sprintf(
			"story points proposals refused two weeks running: %s on %s and %s on %s; the weights have not moved and nobody has been told why",
			outcomes[0].Reason, storyPointsDate(outcomes[0].At), outcomes[1].Reason, storyPointsDate(outcomes[1].At))))
	}
	return out
}

// storyPointsAttentionItem is one info item of the weights kind.
func storyPointsAttentionItem(key, summary string) attention.Item {
	return attention.Item{
		Key:      key,
		Kind:     attention.KindStoryPoints,
		Severity: attention.SeverityLow,
		Summary:  summary,
	}
}

// storyPointsStamp is a record's time in UTC, the part of its item key that keeps
// each record's item apart from the others'.
func storyPointsStamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// storyPointsDate is a record's day in UTC, for the summary a reader sees.
func storyPointsDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
