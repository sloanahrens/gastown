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
		return nil, fmt.Errorf("no beads directory for rig %s: %w", rig, beads.ErrNotFound)
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

	mu       sync.Mutex
	findings map[string]bool
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
// head without a PASS or FAIL, and no line yet is unlabeled. A FAIL's finding
// beads are collected for Context.
func (a *landingAdapter) Label(_ context.Context, item Item) (Labeling, error) {
	rig := item.Refs[refRig]
	if rig == "" {
		rig = landing.RigName
	}
	notes, err := a.notes.Notes(rig, item.ID)
	if err != nil {
		return Labeling{}, fmt.Errorf("reading %s's notes: %w", item.ID, err)
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
		a.count(land.OverseerReviewFindings(notes, item.Refs[refHead]))
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

// count adds the finding beads a FAIL line named to the report's context.
func (a *landingAdapter) count(ids []string) {
	if len(ids) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.findings == nil {
		a.findings = map[string]bool{}
	}
	for _, id := range ids {
		a.findings[id] = true
	}
}

// Context reports the finding beads the FAIL lines named, each once, and
// clears the tally so the next build starts fresh.
func (a *landingAdapter) Context() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.findings))
	for id := range a.findings {
		ids = append(ids, id)
	}
	a.findings = nil
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	return []string{fmt.Sprintf("FAIL review notes named %d finding bead(s): %s", len(ids), strings.Join(ids, ", "))}
}
