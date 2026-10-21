// Package drift is the advisory battery on whether a landing left the docs
// behind. A pass takes every landing since the last pass, reads the doc
// sections closest to the landing's diff, asks Jev one battery about each
// section and appends one line per landing to a log at
// .runtime/jev/drift.jsonl. It never edits a doc, files a bead, comments on a
// bead or changes what gt attention shows: the log is the only thing it
// writes.
//
// Docs are read from the bare clone the submission battery owns, at the
// target branch, never from a town worktree or the rig's mayor/rig checkout.
//
// The pass owns no I/O beyond its log and cursor. The caller supplies the
// landings, the git reads, the optional ranker and the Jev client, so a test
// runs a pass with fakes. Open wires the production versions.
package drift

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	// RigName is the rig whose landings a pass reads.
	RigName = "gastown"

	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-drift"

	// maxHunkRunes caps the diff hunks quoted in each section's state.
	maxHunkRunes = 3000

	// maxFiles caps the changed files listed in the state.
	maxFiles = 60

	// defaultTarget is the branch a landing record names when it names none.
	defaultTarget = "main"
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

// Repo reads the rig's repository from the bare clone the submission battery
// owns. Every ref it is handed is checked before git runs.
type Repo interface {
	// Sync fetches the clone's branches. A pass calls it once, before its first
	// read; its failure stops the pass and leaves the cursor unmoved.
	Sync(ctx context.Context) error
	// Numstat returns `git diff --numstat` for base..head.
	Numstat(ctx context.Context, base, head string) (string, error)
	// Patch returns the zero-context diff of base..head for the files whose
	// changes can drift the docs: not Markdown and not test files.
	Patch(ctx context.Context, base, head string) (string, error)
	// ListDocs returns the Markdown files under docs/ at rev, in path order.
	ListDocs(ctx context.Context, rev string) ([]string, error)
	// ReadDoc returns the file at path at rev.
	ReadDoc(ctx context.Context, rev, path string) (string, error)
}

// Ranker names the doc files closest to a diff, best first. A ranker that
// fails or names none leaves the pass to search the docs itself. *hmRanker
// satisfies it when hm is on the PATH.
type Ranker interface {
	Closest(ctx context.Context, patch string) ([]string, error)
}

// Pass is one run of the drift battery, with every dependency injected.
type Pass struct {
	Asker    Asker
	Landings Landings
	Git      Repo
	// Ranker is optional. Nil searches every doc.
	Ranker Ranker

	// LogPath is the log every answered landing appends to.
	LogPath string
	// CursorPath remembers the last landing a pass asked about.
	CursorPath string

	// Allows says whether the day's Jev spend is still under the cap. The
	// pass asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// Warnings receives a note when the ranker fails and the pass falls back
	// to grepping every doc. Nil writes nothing.
	Warnings io.Writer
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of landings that got a log line this pass.
	Asked int
	// Skipped is the number of landings passed over with no line: their refs
	// were refused, or Jev returned an unusable response for every section
	// they named.
	Skipped int
	// CapReached is true when the pass stopped because the day's cap was
	// spent. The landings it did not reach wait for the next pass, and the
	// landing it stopped in is asked again in full.
	CapReached bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time.
	Started bool
}

// ErrFetch means the clone could not be synced, so the pass asked about
// nothing: the landings behind the failure wait for the next pass.
var ErrFetch = errors.New("the bare clone could not be fetched")

// errSkip means a landing has no state the pass can ask about, so it gets no
// line and the pass moves on. That is the case when its refs are refused, a
// deterministic failure the next pass would hit again, and when Jev gives no
// usable answer for any section it names.
var errSkip = errors.New("landing has no usable state to ask about")

// errCapReached means the day's spend reached the cap before a call.
var errCapReached = errors.New("the daily jev cap is spent")

// Run does one pass. Landings are taken oldest first, and each is logged and
// the cursor advanced before the next, so a pass stopped partway never asks
// about a landing twice. A landing whose diff, docs or Ask fails for any
// reason but a refused ref or an unusable response stops the pass with that
// error, and the next pass reads it again. Run returns ErrBusy when another
// pass holds the run lock.
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
	todo := pending(all, cur)
	if len(todo) == 0 {
		return sum, nil
	}
	if err := p.Git.Sync(ctx); err != nil {
		return sum, fmt.Errorf("%w: %w", ErrFetch, err)
	}

	docs := map[string]*docCache{}
	for _, rec := range todo {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		line, err := p.ask(ctx, rec, docs)
		switch {
		case errors.Is(err, errCapReached):
			sum.CapReached = true
			return sum, nil
		case errors.Is(err, errSkip):
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

// pending keeps the landings after the cursor, oldest first. Every landing
// counts, risk path or not. Ties on the landing time break by bead ID, so the
// cursor orders landings completely.
func pending(all []land.LandingRecord, cur battery.Cursor) []land.LandingRecord {
	var out []land.LandingRecord
	for _, r := range all {
		if r.LandedAt.IsZero() || !after(cur, r.LandedAt, r.BeadID) {
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

// ask reads one landing's diff and the docs at its target, asks one Ask per
// doc section the diff names, and returns the line. A landing whose diff
// names no docs-relevant change gets a line with no sections: the pass read
// it and found nothing for the docs to be stale about.
func (p *Pass) ask(ctx context.Context, rec land.LandingRecord, docs map[string]*docCache) (Line, error) {
	stat, err := p.Git.Numstat(ctx, rec.Base, rec.Head)
	if err != nil {
		return Line{}, readErr(err)
	}
	patch, err := p.Git.Patch(ctx, rec.Base, rec.Head)
	if err != nil {
		return Line{}, readErr(err)
	}
	files := parseNumstat(stat)

	var answers []SectionAnswers
	unanswered := 0
	model := ""
	if strings.TrimSpace(patch) != "" {
		secs, err := p.closest(ctx, rec, patch, docs)
		if err != nil {
			return Line{}, readErr(err)
		}
		hunks := truncate(patch, maxHunkRunes)
		for _, s := range secs {
			if err := p.allowed(); err != nil {
				return Line{}, err
			}
			got, err := p.Asker.Ask(ctx, buildState(files, hunks, s), Questions())
			if errors.Is(err, jev.ErrBadResponse) {
				unanswered++
				continue
			}
			if err != nil {
				return Line{}, err
			}
			model = got.Model
			answers = append(answers, SectionAnswers{Doc: s.Doc, Heading: s.Heading, Answers: answersFrom(got)})
		}
		if len(secs) > 0 && len(answers) == 0 {
			return Line{}, errSkip
		}
	}
	if answers == nil {
		answers = []SectionAnswers{}
	}
	return Line{
		TS:              p.now(),
		Bead:            rec.BeadID,
		Rig:             rec.Rig,
		Head:            rec.Head,
		LandedAt:        rec.LandedAt,
		Agent:           rec.Agent,
		Sections:        answers,
		Unanswered:      unanswered,
		QuestionVersion: Version,
		Model:           model,
	}, nil
}

// closest returns the doc sections the landing's patch names, best first and
// at most maxSections. The ranker's files are searched first; when they name
// no section the diff matches, every doc is searched.
func (p *Pass) closest(ctx context.Context, rec land.LandingRecord, patch string, docs map[string]*docCache) ([]Section, error) {
	terms := identifiers(patch)
	if len(terms) == 0 {
		return nil, nil
	}
	c := docCacheFor(docs, p.Git, "refs/heads/"+target(rec))
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	if p.Ranker != nil {
		paths, err := p.Ranker.Closest(ctx, patch)
		switch {
		case err != nil:
			if p.Warnings != nil {
				fmt.Fprintf(p.Warnings, "jev_drift: ranker failed, searching every doc: %v\n", err)
			}
		case len(paths) > 0:
			if secs := rank(terms, c.sectionsOf(paths)); len(secs) > 0 {
				return secs, nil
			}
		}
	}
	return rank(terms, c.allSections()), nil
}

// allowed returns errCapReached when the day's spend has reached the cap.
func (p *Pass) allowed() error {
	ok, err := p.Allows()
	if err != nil {
		return fmt.Errorf("reading today's spend: %w", err)
	}
	if !ok {
		return errCapReached
	}
	return nil
}

// readErr passes a read failure up, so the pass stops with its cursor on the
// landing and the next pass reads it again: a git error can be transient. A
// refused ref or path is the same every time, so that landing is skipped.
func readErr(err error) error {
	if errors.Is(err, ErrBadRef) {
		return errSkip
	}
	return err
}

// target is the branch a landing lands on, defaultTarget when the record names
// none.
func target(rec land.LandingRecord) string {
	if rec.Target == "" {
		return defaultTarget
	}
	return rec.Target
}

// docCache holds one revision's doc files. A pass reads each revision once:
// the clone is synced once before the first landing, so every landing in the
// pass sees the same docs. A load that fails stops the pass, so the cache is
// never asked to load again in the same pass.
type docCache struct {
	git    Repo
	rev    string
	loaded bool
	paths  []string
	secs   map[string][]Section
}

// docCacheFor returns the cache for rev, creating it on first use.
func docCacheFor(docs map[string]*docCache, git Repo, rev string) *docCache {
	if c, ok := docs[rev]; ok {
		return c
	}
	c := &docCache{git: git, rev: rev}
	docs[rev] = c
	return c
}

// load lists the docs at the revision and splits each into sections.
func (c *docCache) load(ctx context.Context) error {
	if c.loaded {
		return nil
	}
	paths, err := c.git.ListDocs(ctx, c.rev)
	if err != nil {
		return err
	}
	secs := make(map[string][]Section, len(paths))
	for _, path := range paths {
		text, err := c.git.ReadDoc(ctx, c.rev, path)
		if err != nil {
			return err
		}
		secs[path] = splitSections(path, text)
	}
	c.paths, c.secs, c.loaded = paths, secs, true
	return nil
}

// allSections returns every doc's sections, in path order.
func (c *docCache) allSections() []Section {
	return c.sectionsOf(c.paths)
}

// sectionsOf returns the sections of the named docs that the revision holds,
// in the order named.
func (c *docCache) sectionsOf(paths []string) []Section {
	var out []Section
	for _, path := range paths {
		out = append(out, c.secs[path]...)
	}
	return out
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

// buildState returns the state for one doc section: one "field: value" line
// per field, each value JSON-quoted so the model reads it as data. It holds
// the changed paths with counts, the diff hunks and the section, never a
// bead's text.
func buildState(files []fileChange, hunks string, s Section) string {
	if files == nil {
		files = []fileChange{}
	}
	var b strings.Builder
	field := func(name string, v any) { fmt.Fprintf(&b, "%s: %s\n", name, quote(v)) }
	field("files_changed", files)
	field("diff_hunks", hunks)
	field("doc_path", s.Doc)
	field("doc_heading", s.Heading)
	field("doc_section", s.Text)
	return b.String()
}

// quote returns v as JSON on one line. It leaves <, > and & as they are, which
// the default encoder would turn into \u escapes.
func quote(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Every value passed is a string or a slice of plain structs.
	_ = enc.Encode(v)
	return strings.TrimSuffix(buf.String(), "\n")
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}
