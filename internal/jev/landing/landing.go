// Package landing is the advisory second opinion on each risk-path landing.
// A pass takes the landings that touched risk paths since the last pass, asks
// Jev one battery about each, and appends one line per landing to a log that
// `gt jev landing --recent` prints. It never comments on a bead, files an
// attention item or changes what gt attention shows: the log is the only thing
// it writes.
//
// The pass owns no I/O beyond its log and cursor. The caller supplies the
// landings reader, the bead reads, the git diff and the Jev client, so a test
// runs a pass with fakes. Open wires the production versions.
package landing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	// RigName is the rig whose landings a pass reads.
	RigName = "gastown"

	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-landing"

	// maxGoalRunes caps the bead's goal quoted in the state.
	maxGoalRunes = 1200

	// maxFiles caps the changed files listed in the state.
	maxFiles = 60
)

// Asker asks one multi-question Ask. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Landings lists a rig's landing records landed at or after t, oldest first.
// *land.LandingsFile satisfies it.
type Landings interface {
	Since(t time.Time) ([]land.LandingRecord, error)
}

// Beads reads one bead. A pass only reads them.
type Beads interface {
	Show(id string) (*beads.Issue, error)
}

// Differ returns `git diff --numstat` for base..head in the rig's repo.
type Differ interface {
	Numstat(ctx context.Context, base, head string) (string, error)
}

// Pass is one run of the landing battery, with every dependency injected.
type Pass struct {
	Asker    Asker
	Landings Landings
	Beads    Beads
	Git      Differ

	// LogPath is the log every answered landing appends to.
	LogPath string
	// CursorPath remembers the last landing a pass asked about.
	CursorPath string

	// Allows says whether the day's Jev spend is still under the cap. The
	// pass asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of landings that got a log line this pass.
	Asked int
	// Skipped is the number of landings passed over with no line: Jev returned
	// an unusable response for them, or their bead or diff could not be read.
	// An answer built on an unread bead or diff would judge a change the
	// battery never saw, so no line is written. Skipping them means one cannot
	// hold up every landing behind it.
	Skipped int
	// CapReached is true when the pass stopped because the day's cap was
	// spent. The landings it did not reach wait for the next pass.
	CapReached bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time.
	Started bool
}

// Run does one pass. Landings are taken oldest first, and each is logged and
// the cursor advanced before the next, so a pass stopped partway never asks
// about a landing twice. A landing whose Ask fails for any reason but an
// unusable response, or whose bead read fails for any reason but a missing
// bead, stops the pass with that error and is asked again by the next one.
// Run returns ErrBusy when another pass holds the run lock.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	release, err := lockRun(p.CursorPath)
	if err != nil {
		return sum, err
	}
	defer release()

	cur, ok, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		return sum, err
	}
	if !ok {
		sum.Started = true
		return sum, battery.WriteCursor(p.CursorPath, battery.Cursor{Time: p.now()})
	}

	all, err := p.Landings.Since(cur.Time)
	if err != nil {
		return sum, fmt.Errorf("listing landings since %s: %w", cur.Time.Format(time.RFC3339), err)
	}
	for _, rec := range pending(all, cur) {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		allowed, err := p.Allows()
		if err != nil {
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			sum.CapReached = true
			break
		}
		line, err := p.ask(ctx, rec)
		switch {
		case errors.Is(err, errUnreadable), errors.Is(err, jev.ErrBadResponse):
			sum.Skipped++
		case err != nil:
			return sum, err
		default:
			if err := battery.Append(p.LogPath, line); err != nil {
				return sum, err
			}
			sum.Asked++
		}
		if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: rec.LandedAt, ID: rec.BeadID}); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// pending keeps the risk-path landings after the cursor, oldest first. Ties on
// the landing time break by bead ID, so the cursor orders landings completely.
func pending(all []land.LandingRecord, cur battery.Cursor) []land.LandingRecord {
	var out []land.LandingRecord
	for _, r := range all {
		if len(r.RiskPaths) == 0 || r.LandedAt.IsZero() || !after(cur, r.LandedAt, r.BeadID) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LandedAt.Equal(out[j].LandedAt) {
			return out[i].LandedAt.Before(out[j].LandedAt)
		}
		return out[i].BeadID < out[j].BeadID
	})
	return out
}

// after reports whether a landing at landed for bead sorts after the cursor.
func after(cur battery.Cursor, landed time.Time, bead string) bool {
	if landed.Equal(cur.Time) {
		return bead > cur.ID
	}
	return landed.After(cur.Time)
}

// errUnreadable means a landing's bead or diff could not be read, so the pass
// has no state to ask about. The read is not retried: one unreadable landing
// must not hold up the landings behind it, and the landing is already on main.
var errUnreadable = errors.New("landing bead or diff unreadable")

// ask builds the state for one landing, asks the battery and returns the line.
// A bead that does not exist (beads.ErrNotFound) or a diff that cannot be read
// returns errUnreadable, and the pass skips the landing. Any other bead read
// error is infrastructure, such as a Dolt outage: it is returned wrapped, so
// the pass stops before the cursor moves and the next pass asks again.
// A failure caused by ctx ending returns ctx's error, so the next pass
// asks again.
func (p *Pass) ask(ctx context.Context, rec land.LandingRecord) (Line, error) {
	b, err := p.Beads.Show(rec.BeadID)
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return Line{}, fmt.Errorf("reading bead %s: %w", rec.BeadID, err)
	}
	if err != nil || b == nil {
		return Line{}, errUnreadable
	}
	out, err := p.Git.Numstat(ctx, rec.Base, rec.Head)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		return Line{}, errUnreadable
	}

	got, err := p.Asker.Ask(ctx, buildState(rec, b.Title, goalSection(b.Description), parseNumstat(out)), Questions())
	if err != nil {
		return Line{}, err
	}
	return Line{
		TS:              p.now(),
		Bead:            rec.BeadID,
		Rig:             rec.Rig,
		Head:            rec.Head,
		LandedAt:        rec.LandedAt,
		Agent:           rec.Agent,
		Answers:         answersFrom(got),
		QuestionVersion: Version,
		Model:           got.Model,
	}, nil
}

// fileChange is one changed file and its line counts. A binary file counts 0.
type fileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// parseNumstat reads `git diff --numstat` output into paths with counts,
// capped at maxFiles. Numstat output holds no hunk text.
func parseNumstat(out string) []fileChange {
	var files []fileChange
	for _, l := range strings.Split(out, "\n") {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		// A binary file reports "-" for both counts, which Atoi fails on and
		// leaves 0.
		added, _ := strconv.Atoi(parts[0])
		removed, _ := strconv.Atoi(parts[1])
		files = append(files, fileChange{Path: parts[2], Added: added, Removed: removed})
		if len(files) == maxFiles {
			break
		}
	}
	return files
}

// buildState returns the state for one landing: one "field: value" line per
// field, each value JSON-quoted so the model reads it as data. The files are
// paths with counts, never diff content.
func buildState(rec land.LandingRecord, title, goal string, files []fileChange) string {
	if files == nil {
		files = []fileChange{}
	}
	var b strings.Builder
	field := func(name string, v any) { fmt.Fprintf(&b, "%s: %s\n", name, quote(v)) }
	field("bead_title", title)
	field("bead_goal", truncate(goal, maxGoalRunes))
	field("risk_paths", rec.RiskPaths)
	field("om_verdict", rec.OMVerdict)
	field("om_score", rec.OMScore)
	field("gate_result", rec.GateResult)
	field("agent", rec.Agent)
	field("files_changed", files)
	return b.String()
}

// goalSection is the body of a bead's "## Goal" section, up to the next "##"
// heading, without the dispatcher's attachment lines. A bead with no such
// section has no goal.
func goalSection(desc string) string {
	lines := strings.Split(strings.TrimSpace(beads.StripAttachmentFields(desc)), "\n")
	for i, l := range lines {
		if !strings.EqualFold(strings.TrimSpace(l), "## Goal") {
			continue
		}
		rest := lines[i+1:]
		for j, l := range rest {
			if strings.HasPrefix(strings.TrimSpace(l), "## ") {
				rest = rest[:j]
				break
			}
		}
		return strings.TrimSpace(strings.Join(rest, "\n"))
	}
	return ""
}

// quote returns v as JSON on one line. It leaves <, > and & as they are, which
// the default encoder would turn into \u escapes.
func quote(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Every value passed is a string, a number or a slice of plain structs.
	_ = enc.Encode(v)
	return strings.TrimSuffix(buf.String(), "\n")
}

// truncate cuts s to at most n runes, marking a cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}
