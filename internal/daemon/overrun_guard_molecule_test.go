package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
)

const (
	molWorkBead = "gt-aaa11"
	molRoot     = "gt-wisp-root"
)

var molSteps = []string{"gt-wisp-s1", "gt-wisp-s2", "gt-wisp-s3"}

// molStore is beadsfake behind overrunHandBackStore, with Children failing on
// demand to stand for a molecule that cannot be read, Show failing its next
// showFails reads of the work bead to stand for a bead that cannot be read,
// and a close that refuses closeRefused to stand for bd skipping an issue.
type molStore struct {
	*beadsfake.Fake
	childrenErr  error
	showFails    int
	closeRefused map[string]bool
}

func (s *molStore) Show(id string) (*beads.Issue, error) {
	if id == molWorkBead && s.showFails > 0 {
		s.showFails--
		return nil, errors.New("dolt timeout")
	}
	return s.Fake.Show(id)
}

// ForceCloseWithReason closes the ids it is not told to refuse and reports the
// rest as a *beads.PartialCloseError, as Close does when bd exits 0 and skips
// an issue.
func (s *molStore) ForceCloseWithReason(reason string, ids ...string) error {
	if len(s.closeRefused) == 0 {
		return s.Fake.ForceCloseWithReason(reason, ids...)
	}
	var closed, refused []string
	for _, id := range ids {
		if s.closeRefused[id] {
			refused = append(refused, id)
		} else {
			closed = append(closed, id)
		}
	}
	if err := s.Fake.ForceCloseWithReason(reason, closed...); err != nil {
		return err
	}
	return &beads.PartialCloseError{Closed: closed, NotClosed: refused}
}

func (s *molStore) Children(parentID string) ([]*beads.Issue, error) {
	if s.childrenErr != nil {
		return nil, s.childrenErr
	}
	return s.Fake.Children(parentID)
}

// newMolStore seeds the work bead the stop acts on: hooked to the stopped
// seat, carrying a molecule of three chained step wisps, the first claimed by
// the seat as a running polecat has it.
func newMolStore(t *testing.T) *molStore {
	t.Helper()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	bd.Seed(beads.Issue{ID: molWorkBead, Title: "the work", Status: "hooked", Type: "task",
		Assignee:    stopSeat.Rig + "/polecats/" + stopSeat.Polecat,
		Description: "attached_molecule: " + molRoot + "\nattached_formula: mol-polecat-work\n"})
	for i, id := range molSteps {
		status := "open"
		if i == 0 {
			status = "in_progress"
		}
		bd.Seed(beads.Issue{ID: id, Title: fmt.Sprintf("step %d", i+1), Status: status, Type: "task",
			Assignee: stopSeat.Rig + "/polecats/" + stopSeat.Polecat})
	}
	bd.Seed(beads.Issue{ID: molRoot, Title: "mol-polecat-work", Status: "open", Type: "molecule"})
	for i, id := range molSteps {
		if err := bd.AddTypedDependency(id, molRoot, "parent-child"); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			if err := bd.AddDependency(id, molSteps[i-1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := bd.AddDependency(molRoot, molWorkBead); err != nil {
		t.Fatal(err)
	}
	return &molStore{Fake: bd}
}

func (s *molStore) status(t *testing.T, id string) string {
	t.Helper()
	is, err := s.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	return is.Status
}

// handBack runs the production hand-back over s and returns its log.
func (s *molStore) handBack(t *testing.T) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	logf := func(format string, args ...any) { fmt.Fprintf(&buf, format+"\n", args...) }
	err := handBackEndingMolecule(context.Background(), s, logf, stopSeat, 20*time.Minute, "")
	return buf.String(), err
}

func commentTexts(t *testing.T, s *molStore, id string) []string {
	t.Helper()
	cs, err := s.Comments(id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		out = append(out, c.Text)
	}
	return out
}

func TestHandBackEndingMolecule_EndsTheMoleculeAndHandsTheBeadBack(t *testing.T) {
	t.Parallel()
	s := newMolStore(t)

	log, err := s.handBack(t)
	if err != nil {
		t.Fatalf("hand-back: %v", err)
	}

	for _, id := range append([]string{molRoot}, molSteps...) {
		if got := s.status(t, id); got != "closed" {
			t.Errorf("%s status = %q, want closed", id, got)
		}
	}
	work, err := s.Show(molWorkBead)
	if err != nil {
		t.Fatal(err)
	}
	if work.Status != "open" || work.Assignee != "" {
		t.Errorf("bead = status %q assignee %q, want open and unassigned", work.Status, work.Assignee)
	}
	if !beads.HasLabel(work, polecat.NeedsSplitLabel) {
		t.Errorf("bead labels %v lack %s", work.Labels, polecat.NeedsSplitLabel)
	}
	if !strings.Contains(log, "ended molecule "+molRoot) {
		t.Errorf("log does not record the ended molecule:\n%s", log)
	}
}

// The branch, the label and the comment are the hand-back's own: a bead with a
// molecule comes out of it exactly as one without.
func TestHandBackEndingMolecule_HandBackIsUnchanged(t *testing.T) {
	t.Parallel()
	with := newMolStore(t)
	if _, err := with.handBack(t); err != nil {
		t.Fatalf("hand-back with a molecule: %v", err)
	}

	bare := newMolStore(t)
	if err := bare.Update(molWorkBead, beads.UpdateOptions{Description: strPtr("no workflow here")}); err != nil {
		t.Fatal(err)
	}
	if _, err := bare.handBack(t); err != nil {
		t.Fatalf("hand-back without a molecule: %v", err)
	}

	got, want := commentTexts(t, with, molWorkBead), commentTexts(t, bare, molWorkBead)
	if len(got) != 1 || len(want) != 1 || got[0] != want[0] {
		t.Fatalf("comments = %q, want exactly %q", got, want)
	}
	for _, frag := range []string{"polecat stopped after 20m0s", "work kept on branch " + stopSeat.Branch, "remove the needs-split label"} {
		if !strings.Contains(got[0], frag) {
			t.Errorf("comment %q lacks %q", got[0], frag)
		}
	}
	if st := bare.status(t, molRoot); st != "open" {
		t.Errorf("a bead naming no molecule still had %s ended: status %q", molRoot, st)
	}
}

func TestHandBackEndingMolecule_UnreadableMoleculeIsLoggedAndTheBeadStillHandedBack(t *testing.T) {
	t.Parallel()
	s := newMolStore(t)
	s.childrenErr = errors.New("dolt unavailable")

	log, err := s.handBack(t)
	if err != nil {
		t.Fatalf("hand-back failed because the molecule was unreadable: %v", err)
	}

	work, err := s.Show(molWorkBead)
	if err != nil {
		t.Fatal(err)
	}
	if work.Status != "open" || !beads.HasLabel(work, polecat.NeedsSplitLabel) {
		t.Errorf("bead = status %q labels %v, want open with needs-split", work.Status, work.Labels)
	}
	if got := commentTexts(t, s, molWorkBead); len(got) != 1 {
		t.Errorf("comments = %q, want the one hand-back comment", got)
	}
	if got := s.status(t, molRoot); got != "open" {
		t.Errorf("molecule root status = %q, want it left open", got)
	}
	if !strings.Contains(log, "left open") || !strings.Contains(log, "dolt unavailable") {
		t.Errorf("log does not name the unreadable molecule:\n%s", log)
	}
}

// A work bead that cannot be read names no molecule: the hand-back goes ahead
// and ends nothing, and says why.
func TestHandBackEndingMolecule_UnreadableWorkBeadIsLoggedAndStillHandedBack(t *testing.T) {
	t.Parallel()
	s := newMolStore(t)
	s.showFails = 1

	log, err := s.handBack(t)
	if err != nil {
		t.Fatalf("hand-back failed because the work bead was unreadable once: %v", err)
	}

	work, err := s.Show(molWorkBead)
	if err != nil {
		t.Fatal(err)
	}
	if work.Status != "open" || work.Assignee != "" || !beads.HasLabel(work, polecat.NeedsSplitLabel) {
		t.Errorf("bead = status %q assignee %q labels %v, want open, unassigned, needs-split", work.Status, work.Assignee, work.Labels)
	}
	for _, id := range append([]string{molRoot}, molSteps...) {
		if got := s.status(t, id); got == "closed" {
			t.Errorf("%s closed although the work bead named no molecule", id)
		}
	}
	if !strings.Contains(log, "cannot read "+molWorkBead) || !strings.Contains(log, "dolt timeout") {
		t.Errorf("log does not name the unreadable work bead:\n%s", log)
	}
}

// bd skips an issue it refuses and exits 0: the steps it closed stay closed,
// the root stays open and the log names what was left. The hand-back stands.
func TestHandBackEndingMolecule_PartialCloseIsLoggedAndTheBeadStillHandedBack(t *testing.T) {
	t.Parallel()
	s := newMolStore(t)
	s.closeRefused = map[string]bool{molRoot: true, molSteps[1]: true}

	log, err := s.handBack(t)
	if err != nil {
		t.Fatalf("hand-back failed because the molecule closed only in part: %v", err)
	}

	work, err := s.Show(molWorkBead)
	if err != nil {
		t.Fatal(err)
	}
	if work.Status != "open" || !beads.HasLabel(work, polecat.NeedsSplitLabel) {
		t.Errorf("bead = status %q labels %v, want open with needs-split", work.Status, work.Labels)
	}
	for id, want := range map[string]string{molSteps[0]: "closed", molSteps[1]: "open", molSteps[2]: "closed", molRoot: "open"} {
		if got := s.status(t, id); got != want {
			t.Errorf("%s status = %q, want %q", id, got, want)
		}
	}
	if !strings.Contains(log, "left open") || !strings.Contains(log, "closed 2 of 4") || strings.Contains(log, "ended molecule") {
		t.Errorf("log does not name the half-ended molecule:\n%s", log)
	}
}

// What a refused close left is finished by a repeated hand-back, the repeat the
// stop makes when it cannot save its progress; the stop does not repeat one on
// account of the molecule alone.
func TestHandBackEndingMolecule_RepeatFinishesAMoleculeLeftOpen(t *testing.T) {
	t.Parallel()
	s := newMolStore(t)
	s.closeRefused = map[string]bool{molRoot: true}
	if _, err := s.handBack(t); err != nil {
		t.Fatal(err)
	}
	if got := s.status(t, molRoot); got != "open" {
		t.Fatalf("molecule root status = %q after the refused close, want open", got)
	}

	s.closeRefused = nil
	log, err := s.handBack(t)
	if err != nil {
		t.Fatalf("repeat hand-back: %v", err)
	}
	for _, id := range append([]string{molRoot}, molSteps...) {
		if got := s.status(t, id); got != "closed" {
			t.Errorf("%s status = %q after the repeat, want closed", id, got)
		}
	}
	if !strings.Contains(log, "ended molecule "+molRoot+" of "+molWorkBead+" (1 bead(s) closed)") {
		t.Errorf("log does not record the repeat ending the molecule:\n%s", log)
	}
}

// A hand-back that is skipped (the bead is closed or another polecat's) ends
// no molecule: it belongs to whoever holds the bead now.
func TestHandBackEndingMolecule_SkippedHandBackLeavesTheMolecule(t *testing.T) {
	t.Parallel()
	s := newMolStore(t)
	other := "gastown/polecats/someone-else"
	if err := s.Update(molWorkBead, beads.UpdateOptions{Assignee: &other}); err != nil {
		t.Fatal(err)
	}

	_, err := s.handBack(t)
	if !errors.Is(err, polecat.ErrHandBackSkipped) {
		t.Fatalf("hand-back error = %v, want ErrHandBackSkipped", err)
	}
	for _, id := range append([]string{molRoot}, molSteps...) {
		if got := s.status(t, id); got == "closed" {
			t.Errorf("%s closed although the hand-back was skipped", id)
		}
	}
}

// The stop repeats a hand-back it could not finish; the second pass finds the
// molecule ended and closes nothing.
func TestHandBackEndingMolecule_RepeatedHandBackIsQuiet(t *testing.T) {
	t.Parallel()
	s := newMolStore(t)
	if _, err := s.handBack(t); err != nil {
		t.Fatal(err)
	}
	log, err := s.handBack(t)
	if err != nil {
		t.Fatalf("repeat hand-back: %v", err)
	}
	if strings.Contains(log, "ended molecule") || strings.Contains(log, "left open") {
		t.Errorf("repeat hand-back touched the ended molecule:\n%s", log)
	}
}

func strPtr(s string) *string { return &s }
