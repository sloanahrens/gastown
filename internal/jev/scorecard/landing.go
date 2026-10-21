package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev/landing"
	"github.com/steveyegge/gastown/internal/land"
)

// errNoRigBeads is a rig whose beads directory does not exist. It wraps
// beads.ErrNotFound, so Show reads it as no bead to find, and Label tells it
// from a bead missing in a rig that has a directory.
var errNoRigBeads = fmt.Errorf("no beads directory: %w", beads.ErrNotFound)

// LandingBattery is the landing adapter's name: the --battery value.
const LandingBattery = "landing"

// Refs the landing adapter keeps in Item.Refs.
const (
	refRig  = "rig"
	refHead = "head"
)

// Rules the landing adapter names in the --json report's lines.
const (
	ruleLandingReviewPass = "landing:review-pass"
	ruleLandingReviewFail = "landing:review-fail"
)

func init() {
	Register(LandingBattery, func(townRoot string) Adapter {
		return &landingAdapter{
			log:   landing.LogPath(townRoot),
			notes: &townNotes{town: townRoot, stores: map[string]*beads.Beads{}},
		}
	})
}

// landingNotes reads one bead's notes from its rig's beads store.
type landingNotes interface {
	Notes(rig, bead string) (string, error)
}

// townNotes reads each rig through a bd-backed store, one per rig per adapter.
type townNotes struct {
	town   string
	stores map[string]*beads.Beads
}

func (t *townNotes) Notes(rig, bead string) (string, error) {
	issue, err := t.Show(rig, bead)
	if err != nil {
		return "", err
	}
	return issue.Notes, nil
}

// Show reads one bead from its rig's beads store. A rig whose beads directory
// does not exist has no bead to find, so it reads as not found.
func (t *townNotes) Show(rig, bead string) (*beads.Issue, error) {
	b, err := t.store(rig)
	if err != nil {
		return nil, err
	}
	return b.Show(bead)
}

// ShowAll reads several of a rig's beads in as few calls as it can. A rig whose
// store cannot be opened, a bead it does not hold and a failed call all leave
// the bead out of the result, for the caller to read one at a time.
func (t *townNotes) ShowAll(rig string, ids []string) map[string]*beads.Issue {
	b, err := t.store(rig)
	if err != nil {
		return nil
	}
	return showAll(b, ids)
}

// rigShowAller reads several of a rig's beads at once. townNotes is one.
type rigShowAller interface {
	ShowAll(rig string, ids []string) map[string]*beads.Issue
}

// List reads the rig's beads matching opts. A rig with no beads directory fails
// the listing rather than listing none: a rule that reads an empty list would
// label as if it had looked.
func (t *townNotes) List(rig string, opts beads.ListOptions) ([]*beads.Issue, error) {
	b, err := t.store(rig)
	if err != nil {
		return nil, err
	}
	return b.List(opts)
}

// store returns the rig's beads store, opened on first use.
func (t *townNotes) store(rig string) (*beads.Beads, error) {
	if b, ok := t.stores[rig]; ok {
		return b, nil
	}
	dir := doltserver.FindRigBeadsDir(t.town, rig)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("rig %s: %w", rig, errNoRigBeads)
	} else if err != nil {
		return nil, fmt.Errorf("reading the beads directory for rig %s: %w", rig, err)
	}
	b := beads.New(dir)
	t.stores[rig] = b
	return b, nil
}

// landingAdapter labels each logged landing from the overseer's review of the
// head it logged. The landing log writes landing.Line, whose answers carry the
// fields Answer reads; its confidence is not scored.
type landingAdapter struct {
	log   string
	notes landingNotes

	// findings gathers the finding beads Label sees, for Context.
	findings findingTally

	// batched holds the notes Prefetch read in one batch per rig, by rig and
	// bead. Label reads a bead missing from it the one-at-a-time way.
	batched map[string]string
}

// Prefetch reads the notes of every landed bead in one call per rig.
func (a *landingAdapter) Prefetch(_ context.Context, items []Item) {
	a.batched = nil
	all, ok := a.notes.(rigShowAller)
	if !ok {
		return
	}
	byRig := map[string][]string{}
	for _, it := range items {
		byRig[landingRig(it)] = append(byRig[landingRig(it)], it.ID)
	}
	a.batched = map[string]string{}
	for rig, ids := range byRig {
		for id, issue := range all.ShowAll(rig, ids) {
			a.batched[rig+"/"+id] = issue.Notes
		}
	}
}

// landingRig is the rig a landing item's bead lives in.
func landingRig(item Item) string {
	if rig := item.Refs[refRig]; rig != "" {
		return rig
	}
	return landing.RigName
}

// findingTally is the finding beads the FAIL lines named, each once. Label adds
// to it as it walks the items and Context drains it, so one Build reports the
// findings of its own labeling and no earlier one.
type findingTally struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (t *findingTally) add(ids []string) {
	if len(ids) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ids == nil {
		t.ids = map[string]bool{}
	}
	for _, id := range ids {
		t.ids[id] = true
	}
}

// drain returns the sorted ids and empties the tally.
func (t *findingTally) drain() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.ids))
	for id := range t.ids {
		ids = append(ids, id)
	}
	t.ids = nil
	slices.Sort(ids)
	return ids
}

func (a *landingAdapter) Battery() string { return LandingBattery }
func (a *landingAdapter) LogPath() string { return a.log }

// landingLine is the part of a landing.Line the scorecard reads.
type landingLine struct {
	TS      time.Time             `json:"ts"`
	Bead    string                `json:"bead"`
	Rig     string                `json:"rig"`
	Head    string                `json:"head"`
	Model   string                `json:"model"`
	Answers map[QuestionID]Answer `json:"answers"`
}

func (a *landingAdapter) Parse(raw []byte) (Item, error) {
	var line landingLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return Item{}, err
	}
	if line.Bead == "" || line.Head == "" || line.TS.IsZero() {
		return Item{}, errors.New("landing line needs a bead, a head and a ts")
	}
	rig := line.Rig
	if rig == "" {
		rig = landing.RigName
	}
	return Item{
		ID:      line.Bead,
		Time:    line.TS,
		Model:   line.Model,
		Answers: line.Answers,
		Refs:    map[string]string{refRig: rig, refHead: line.Head},
	}, nil
}

// Label reads the work bead's notes. A PASS of the logged head labels
// needs_review_now and risk false: the overseer read the diff and let it stand.
// A FAIL labels both true: the diff should have been read before the wake, and
// the landing was at least level 2 risk. A WAIVED or AUDITED line leaves the
// head without a PASS or FAIL, and no line yet is unlabeled, and so is a
// landing whose bead no longer exists in its rig. A rig with no beads
// directory fails the report, like any other failure to read the notes: it
// would otherwise leave every landing of that rig unlabeled with a clean exit.
// A FAIL's finding beads are collected for Context.
func (a *landingAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	rig := landingRig(item)
	notes, batched := a.batched[rig+"/"+item.ID]
	if !batched {
		var err error
		notes, err = a.notes.Notes(rig, item.ID)
		if errors.Is(err, beads.ErrNotFound) && !errors.Is(err, errNoRigBeads) {
			return Labeling{}, nil
		}
		if err != nil {
			return Labeling{}, fmt.Errorf("reading %s's notes: %w", item.ID, err)
		}
	}
	verdict, ok := land.OverseerReviewVerdict(notes, item.Refs[refHead])
	if !ok {
		return Labeling{}, nil
	}
	failed := verdict == land.OverseerReviewFail
	if !failed && verdict != land.OverseerReviewPass {
		return Labeling{}, nil
	}
	rule := ruleLandingReviewPass
	if failed {
		rule = ruleLandingReviewFail
		a.findings.add(land.OverseerReviewFindings(notes, item.Refs[refHead]))
	}
	return Labeling{
		Labeled: true,
		Labels: map[QuestionID]bool{
			QuestionID(landing.QNeedsReviewNow): failed,
			QuestionID(landing.QRisk):           failed,
		},
		Rule: rule,
	}, nil
}

// Context reports the finding beads the FAIL lines named, each once, and
// clears the tally so the next build starts fresh.
func (a *landingAdapter) Context() []string {
	ids := a.findings.drain()
	if len(ids) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("FAIL review notes named %d finding bead(s): %s", len(ids), strings.Join(ids, ", "))}
}
