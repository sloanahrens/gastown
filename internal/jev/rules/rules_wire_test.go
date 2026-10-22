package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/land"
)

// fakeBeads lists canned beads: the held ones for any open-style query, the
// closed ones for the closed query. A closed bead comes back as the landing
// left it: land.recordBead removes the ready-to-land label in the update that
// closes a landed bead, so the lister drops the label from one, and a label
// filter then leaves it out, as the store does.
type fakeBeads struct {
	held, closed []*beads.Issue
}

func (f fakeBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if opts.Status != "closed" {
		return filterLabel(f.held, opts.Label), nil
	}
	var closed []*beads.Issue
	for _, is := range f.closed {
		c := *is
		if _, landed := land.ParseLandingRecord(is.Notes); landed {
			c.Labels = nil
		}
		closed = append(closed, &c)
	}
	return filterLabel(closed, opts.Label), nil
}

// filterLabel keeps the beads carrying label, or every bead when label is empty.
func filterLabel(list []*beads.Issue, label string) []*beads.Issue {
	if label == "" {
		return list
	}
	var out []*beads.Issue
	for _, is := range list {
		if beads.HasLabel(is, label) {
			out = append(out, is)
		}
	}
	return out
}

// submittedBead is a bead whose live READY TO LAND block was stamped at at.
func submittedBead(id string, at time.Time) *beads.Issue {
	notes := land.FormatReadyNote(land.Work{
		Branch: "polecat/bullet/" + id + "+abc", Head: "abc1234", Target: "main",
		Worker: "bullet", Submitted: at,
	})
	return &beads.Issue{ID: id, Notes: notes, Labels: []string{land.LabelReadyToLand}}
}

// landedBead is a submission the landing worker landed at at: closed with the
// landing's close reason, and the LANDING RECORD block after its READY TO LAND
// block.
func landedBead(id string, at time.Time) *beads.Issue {
	is := submittedBead(id, at.Add(-time.Minute))
	rec := land.LandingRecord{
		LandedCommit: "def5678", PatchID: "p1", Target: "main", Base: "main",
		Branch: "polecat/bullet/" + id + "+abc", Head: "abc1234", LandedAt: at,
	}
	is.Notes += "\n" + rec.NoteBlock()
	is.Status = "closed"
	is.CloseReason = rec.CloseReason()
	return is
}

// TestRigDonesListsSubmissionsAndRefusals checks the done source reads both
// records: submissions still held, submissions closed since the cursor, and
// revert-guard refusals of a configured rig. Anything older than the cursor,
// of another kind, or of an unconfigured rig is left out.
func TestRigDonesListsSubmissionsAndRefusals(t *testing.T) {
	t.Parallel()
	cursor := fixedNow.Add(-30 * time.Minute)
	lister := rigDones{
		stores: map[string]beadLister{"gastown": fakeBeads{
			held: []*beads.Issue{
				submittedBead("gt-open", fixedNow.Add(-10*time.Minute)),
				submittedBead("gt-old", fixedNow.Add(-2*time.Hour)),
			},
			closed: []*beads.Issue{landedBead("gt-landed", fixedNow.Add(-20*time.Minute))},
		}},
		refusals: func() ([]attention.Refusal, error) {
			return []attention.Refusal{
				{TS: fixedNow.Add(-5 * time.Minute), Bead: "gt-refused", Rig: "gastown", Worker: "bullet", Kind: attention.KindRevertGuard},
				{TS: fixedNow.Add(-5 * time.Minute), Bead: "gt-other-kind", Rig: "gastown", Kind: "something-else"},
				{TS: fixedNow.Add(-5 * time.Minute), Bead: "gt-other-rig", Rig: "elsewhere", Kind: attention.KindRevertGuard},
				{TS: fixedNow.Add(-2 * time.Hour), Bead: "gt-stale", Rig: "gastown", Kind: attention.KindRevertGuard},
			}, nil
		},
	}

	got, err := lister.Done(cursor)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]Done{}
	for _, d := range got {
		ids[d.Bead] = d
	}
	if len(got) != 3 {
		t.Fatalf("done = %+v, want gt-open, gt-landed and gt-refused", got)
	}
	for _, want := range []string{"gt-open", "gt-landed", "gt-refused"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("done is missing %s: %+v", want, got)
		}
	}
	if d := ids["gt-refused"]; d.Polecat != "bullet" || d.Rig != "gastown" {
		t.Errorf("refusal done = %+v, want the worker and rig the ledger named", d)
	}
	if d := ids["gt-open"]; d.Polecat != "bullet" || !d.SubmittedAt.Equal(fixedNow.Add(-10*time.Minute).Truncate(time.Second)) {
		t.Errorf("submission done = %+v, want the worker and the Submitted stamp", d)
	}
}

// TestDoneListsLandedBeadAfterLabelRemoval checks a submission that landed
// before the pass is still listed, though the landing removed its
// ready-to-land label in the update that closed it.
func TestDoneListsLandedBeadAfterLabelRemoval(t *testing.T) {
	t.Parallel()
	lister := rigDones{
		stores: map[string]beadLister{"gastown": fakeBeads{
			closed: []*beads.Issue{landedBead("gt-landed", fixedNow.Add(-20*time.Minute))},
		}},
		refusals: func() ([]attention.Refusal, error) { return nil, nil },
	}
	got, err := lister.Done(fixedNow.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Bead != "gt-landed" {
		t.Fatalf("done = %+v, want the landed submission gt-landed", got)
	}
}

// TestDoneSkipsClosedBeadThatNeverLanded checks a bead closed since the cursor
// without the ready-to-land label and without a landed close reason is not a
// submission, though its notes still hold a READY TO LAND block.
func TestDoneSkipsClosedBeadThatNeverLanded(t *testing.T) {
	t.Parallel()
	abandoned := submittedBead("gt-abandoned", fixedNow.Add(-20*time.Minute))
	abandoned.Labels = nil
	abandoned.Status = "closed"
	abandoned.CloseReason = "no-changes: nothing to implement"
	lister := rigDones{
		stores: map[string]beadLister{"gastown": fakeBeads{
			closed: []*beads.Issue{abandoned},
		}},
		refusals: func() ([]attention.Refusal, error) { return nil, nil },
	}
	got, err := lister.Done(fixedNow.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("done = %+v, want nothing for a closed bead that never landed", got)
	}
}

// TestRigDonesDedupesABeadListedTwice checks a bead both held and closed since
// the cursor is one submission, not two.
func TestRigDonesDedupesABeadListedTwice(t *testing.T) {
	t.Parallel()
	b := submittedBead("gt-twice", fixedNow.Add(-10*time.Minute))
	lister := rigDones{
		stores:   map[string]beadLister{"gastown": fakeBeads{held: []*beads.Issue{b}, closed: []*beads.Issue{b}}},
		refusals: func() ([]attention.Refusal, error) { return nil, nil },
	}
	got, err := lister.Done(fixedNow.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("done = %+v, want one submission", got)
	}
}

// TestOpenValidatesSettings checks the pass refuses a price it cannot use, and
// a rig with no beads directory, before any call is made.
func TestOpenValidatesSettings(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	okClient := func(opts ...jev.Option) (*jev.Client, error) {
		return jev.NewClient(append(opts, jev.WithKey("test-key"))...)
	}
	listerOf := func(string) beadLister { return fakeBeads{} }

	if _, err := open(town, Settings{Rigs: []string{"gastown"}}, nil, okClient, listerOf); err == nil ||
		!strings.Contains(err.Error(), "usd_per_billion_tokens") {
		t.Errorf("zero price: err = %v, want the price named", err)
	}
	if _, err := open(town, Settings{Rigs: []string{"gastown"}, USDPerBillionTokens: 3, MaxUSDPerDay: -1}, nil, okClient, listerOf); err == nil ||
		!strings.Contains(err.Error(), "max_usd_per_day") {
		t.Errorf("negative cap: err = %v, want the cap named", err)
	}
	if _, err := open(town, Settings{Rigs: []string{"gastown"}, USDPerBillionTokens: 3}, nil, okClient, listerOf); err == nil ||
		!strings.Contains(err.Error(), "no beads directory for rig gastown") {
		t.Errorf("rig without beads: err = %v, want the rig named", err)
	}
}

// TestOpenWiresTheProductionPass checks Open builds the pass over the rig's
// beads and the town's paths, with the default cap when none is named.
func TestOpenWiresTheProductionPass(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	beadsDir := filepath.Join(town, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var listed string
	listerOf := func(dir string) beadLister { listed = dir; return fakeBeads{} }
	okClient := func(opts ...jev.Option) (*jev.Client, error) {
		return jev.NewClient(append(opts, jev.WithKey("test-key"))...)
	}

	p, err := open(town, Settings{Rigs: []string{"gastown"}, USDPerBillionTokens: 3}, nil, okClient, listerOf)
	if err != nil {
		t.Fatal(err)
	}
	if listed != beadsDir {
		t.Errorf("lister opened %q, want the rig's beads %q", listed, beadsDir)
	}
	if p.LogPath != LogPath(town) || p.CursorPath != CursorPath(town) {
		t.Errorf("paths = %q, %q; want the town's rules log and cursor", p.LogPath, p.CursorPath)
	}
	ok, err := p.Allows()
	if err != nil || !ok {
		t.Errorf("Allows() = %v, %v; want true with nothing spent", ok, err)
	}
}

// TestLocateWorktree checks a polecat's worktree is found under its rig
// directory, and that a polecat with no checkout is not guessed at.
func TestLocateWorktree(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	wt := filepath.Join(town, "gastown", "polecats", "bullet", "gastown")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := locateWorktree(town, "gastown", "bullet"); got != wt {
		t.Errorf("locateWorktree = %q, want %q", got, wt)
	}
	if got := locateWorktree(town, "gastown", "nobody"); got != "" {
		t.Errorf("locateWorktree for a polecat with no checkout = %q, want empty", got)
	}
}
