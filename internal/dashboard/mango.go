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
	Targets       []mangoTarget `json:"targets"`
}

// mangoTarget is one target as the file writes it. A target skipped for too few
// positives carries its name and its positives with no box, and the writer's
// not-yet verdict.
type mangoTarget struct {
	Target      string  `json:"target"`
	Positives   int     `json:"positives"`
	Skipped     bool    `json:"skipped"`
	BestBox     string  `json:"best_box"`
	HoldoutN    int     `json:"holdout_n"`
	HoldoutRate float64 `json:"holdout_rate"`
	Baseline    float64 `json:"baseline"`
	Verdict     string  `json:"verdict"`
}

// MangoTarget is one target as the page draws it, every string stripped and
// capped. HoldoutRate and Baseline are fractions of the holdout and of the whole
// set the box was measured against.
type MangoTarget struct {
	Target      string  `json:"target"`
	Positives   int     `json:"positives"`
	Skipped     bool    `json:"skipped,omitempty"`
	BestBox     string  `json:"best_box,omitempty"`
	HoldoutN    int     `json:"holdout_n"`
	HoldoutRate float64 `json:"holdout_rate"`
	Baseline    float64 `json:"baseline"`
	Verdict     string  `json:"verdict"` // earning, no-signal or not-yet
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
	Items         int           `json:"items"`
	SnapshotHours int           `json:"snapshot_hours"`
	Targets       []MangoTarget `json:"targets"`
	Counts        MangoCounts   `json:"counts"`
}

// ReadMango returns the town-metrics panel in dir as it stands at now. A panel
// that is not there reads as missing, one the reader cannot use as error, and
// one generated more than mangoStale before now as stale.
func ReadMango(dir string, now time.Time) *Mango {
	m := &Mango{At: now, Targets: []MangoTarget{}}
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
	m.GeneratedAt = &gen
	m.Items, m.SnapshotHours = p.Items, p.SnapshotHours
	if note := m.addTargets(p.Targets); note != "" {
		m.State, m.Note = MangoError, note
		return m
	}
	if now.Sub(gen) > mangoStale {
		m.State, m.Note = MangoStale, "stale"
		return m
	}
	m.State, m.Note = MangoFresh, "fresh"
	return m
}

// addTargets fills the page's rows and verdict counts from the panel's, each
// string stripped and capped by cloudText, the package's cleaner for another
// process's text (cloud.go). It returns a note naming a verdict the reader does
// not know, which is the panel's error state: an unknown word is not a target to
// draw as any of the three the page knows.
func (m *Mango) addTargets(in []mangoTarget) string {
	for _, t := range in {
		row := MangoTarget{
			Target:      cloudText(t.Target, mangoTargetMax),
			Positives:   t.Positives,
			Skipped:     t.Skipped,
			BestBox:     cloudText(t.BestBox, mangoBoxMax),
			HoldoutN:    t.HoldoutN,
			HoldoutRate: t.HoldoutRate,
			Baseline:    t.Baseline,
			Verdict:     strings.ToLower(cloudText(t.Verdict, mangoTargetMax)),
		}
		switch row.Verdict {
		case MangoEarning:
			m.Counts.Earning++
		case MangoNoSignal:
			m.Counts.NoSignal++
		case MangoNotYet:
			m.Counts.NotYet++
		default:
			return fmt.Sprintf("unknown target verdict %q", row.Verdict)
		}
		m.Targets = append(m.Targets, row)
	}
	return ""
}
