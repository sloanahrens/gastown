package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// specCandidateChildrenBeforeGTKM9JT is how many children reads one tick made
// over the eighty-candidate fixture below before the memory landed: one bd call
// per candidate, which was the whole per-bead cost of the scan. Measured at
// 45c3f766, the main this was built on.
const specCandidateChildrenBeforeGTKM9JT = 80

// specCandidateFixture is eighty ready beads across one rig, as the board and
// the tick's ledger see them.
func specCandidateFixture() []*beads.Issue {
	issues := make([]*beads.Issue, 0, specCandidateChildrenBeforeGTKM9JT)
	for i := 0; i < specCandidateChildrenBeforeGTKM9JT; i++ {
		issues = append(issues, &beads.Issue{
			ID:        fmt.Sprintf("gt-cand-%02d", i),
			Title:     fmt.Sprintf("Bead %d", i),
			Type:      "task",
			Status:    "open",
			Priority:  2,
			CreatedAt: "2026-09-29T10:00:00Z",
			UpdatedAt: "2026-09-30T0" + string(rune('0'+i%10)) + ":00:00Z",
		})
	}
	return issues
}

// specCandidateBoardRead is the tick's board read over issues: the production
// intake, with the board the fixture serves.
func specCandidateBoardRead(t *testing.T, issues []*beads.Issue) specBoardRead {
	t.Helper()
	read := specCandidates(specTown(t), 2, func(string) ([]*beads.Issue, error) { return issues, nil })
	if len(read.Errors) != 0 {
		t.Fatalf("board errors = %v", read.Errors)
	}
	return read
}

// specCandidateChildrenTicks reads every candidate's children the way the
// tick's read does, through the memory, and returns how many reads reached the
// store.
func specCandidateChildrenTicks(t *testing.T, tick *specAnnotatedTick, read specBoardRead) int {
	t.Helper()
	tick.observe(read)
	reached := 0
	for _, c := range read.Candidates {
		if _, err := tick.children(c.Spec.ID, func() ([]specdispatch.Child, error) {
			reached++
			return nil, nil
		}); err != nil {
			t.Fatalf("children %s: %v", c.Spec.ID, err)
		}
	}
	tick.save()
	return reached
}

// The board row's updated_at reaches the candidate, because it is what the next
// tick compares a remembered children read against (gt-km9jt).
func TestSpecCandidatesCarryTheBoardRowsUpdatedAt(t *testing.T) {
	t.Parallel()
	read := specCandidateBoardRead(t, specCandidateFixture())
	if len(read.Candidates) != specCandidateChildrenBeforeGTKM9JT {
		t.Fatalf("fixture yielded %d candidates, want %d", len(read.Candidates), specCandidateChildrenBeforeGTKM9JT)
	}
	for _, c := range read.Candidates {
		if c.UpdatedAt == "" {
			t.Errorf("candidate %s carries no updated_at", c.Spec.ID)
		}
	}
}

// A tick over eighty unchanged candidates reads each one's children exactly
// once — the cost the scan had before the memory — and a second tick over the
// same board reads none of them (gt-km9jt).
func TestSpecCandidateChildrenAreReusedWhenTheBoardHasNotMoved(t *testing.T) {
	t.Parallel()
	read := specCandidateBoardRead(t, specCandidateFixture())
	root := t.TempDir()
	tickAt := func() *specAnnotatedTick {
		return newSpecAnnotatedTickAt(root, "daemon-1", func() time.Time { return specTestNow }, nil)
	}

	if got := specCandidateChildrenTicks(t, tickAt(), read); got != specCandidateChildrenBeforeGTKM9JT {
		t.Fatalf("first tick read %d candidates' children, want the %d it always read", got, specCandidateChildrenBeforeGTKM9JT)
	}
	if got := specCandidateChildrenTicks(t, tickAt(), read); got != 0 {
		t.Errorf("second tick over unchanged candidates read %d children, want none", got)
	}
}

// A bead the board shows at a new updated_at is read again, and the verdict the
// tick reaches from the fresh children is the new one (gt-km9jt).
func TestSpecCandidateChildrenAreRereadWhenTheBeadIsEdited(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := specTestNow
	tick := newSpecAnnotatedTickAt(root, "daemon-1", func() time.Time { return now }, nil)

	f := newFakeSpecTown(cleanSpec("gt-box", 1, "2026-09-29T10:00:00Z"))
	f.children["gt-box"] = []specdispatch.Child{{ID: "gt-box.1", Status: "open"}}
	updated := map[string]string{"gt-box": "2026-09-30T10:00:00Z"}
	reads := 0

	env := f.env()
	candidates := env.Candidates
	env.Candidates = func() specBoardRead {
		read := candidates()
		for i := range read.Candidates {
			read.Candidates[i].UpdatedAt = updated[read.Candidates[i].Spec.ID]
		}
		tick.observe(read)
		return read
	}
	children := env.Children
	env.Children = func(id string) ([]specdispatch.Child, error) {
		return tick.children(id, func() ([]specdispatch.Child, error) {
			reads++
			return children(id)
		})
	}

	held := runSpecDispatchCycle(env)
	tick.save()
	if len(held.Skipped) != 1 || !strings.Contains(held.Skipped[0].Line, "container") {
		t.Fatalf("first tick skipped %+v, want the container hold", held.Skipped)
	}
	if reads != 1 {
		t.Fatalf("first tick read %d children, want 1", reads)
	}

	// The operator closes the child and the edit moves the bead's updated_at.
	f.children["gt-box"] = []specdispatch.Child{{ID: "gt-box.1", Status: "closed"}}
	updated["gt-box"] = "2026-09-30T10:05:00Z"

	slung := runSpecDispatchCycle(env)
	tick.save()
	if reads != 2 {
		t.Fatalf("tick after the edit read %d children, want the moved bead's one read", reads)
	}
	if len(slung.Skipped) != 0 || len(f.slung) != 1 {
		t.Errorf("tick after the edit skipped %+v and slung %v, want the freed container dispatched", slung.Skipped, f.slung)
	}
}

// A remembered read older than the lease is read again even though the bead has
// not moved: a child can close without touching its parent's updated_at, so the
// memory is a shortcut with a lease, never the truth (gt-km9jt).
func TestSpecCandidateChildrenExpireWithTheirLease(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := specTestNow
	tickAt := func() *specAnnotatedTick {
		return newSpecAnnotatedTickAt(root, "daemon-1", func() time.Time { return now }, nil)
	}
	read := specCandidateBoardRead(t, specCandidateFixture())

	specCandidateChildrenTicks(t, tickAt(), read)
	now = now.Add(specChildrenLease - time.Second)
	if got := specCandidateChildrenTicks(t, tickAt(), read); got != 0 {
		t.Errorf("a read inside its lease was made again (%d reads)", got)
	}
	now = now.Add(2 * time.Second)
	if got := specCandidateChildrenTicks(t, tickAt(), read); got != len(read.Candidates) {
		t.Errorf("a read past its lease answered %d of %d candidates", len(read.Candidates)-got, len(read.Candidates))
	}
}

// A children read that fails is not remembered: the next tick asks the store
// again rather than answering an unknown child set from nothing (gt-km9jt).
func TestSpecCandidateChildrenFailureIsNotRemembered(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tick := newSpecAnnotatedTickAt(root, "daemon-1", func() time.Time { return specTestNow }, nil)
	read := specCandidateBoardRead(t, specCandidateFixture()[:1])
	tick.observe(read)

	if _, err := tick.children(read.Candidates[0].Spec.ID, func() ([]specdispatch.Child, error) {
		return nil, fmt.Errorf("bd: connection refused")
	}); err == nil {
		t.Fatal("a failed read came back without its error")
	}
	reached := 0
	if _, err := tick.children(read.Candidates[0].Spec.ID, func() ([]specdispatch.Child, error) {
		reached++
		return nil, nil
	}); err != nil {
		t.Fatalf("children: %v", err)
	}
	if reached != 1 {
		t.Errorf("the tick after a failed read asked the store %d times, want 1", reached)
	}
}

// A remembered child set this code did not write is read again: a child with no
// status reads as open to the container rule, so answering it would hold a bead
// the file says nothing trustworthy about (gt-km9jt).
func TestSpecCandidateChildrenFromAnUnknownSchemaAreReadAgain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := specTestNow
	read := specCandidateBoardRead(t, specCandidateFixture()[:1])
	c := read.Candidates[0]

	path := dispatch.SpecAnnotatedPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"owner":"daemon-1","beads":{},"candidates":{%q:{"updated_at":%q,"read_at":%q,"children":[{"id":"gt-box.1"}]}}}`,
		c.Spec.ID, c.UpdatedAt, now.Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	tick := newSpecAnnotatedTickAt(root, "daemon-1", func() time.Time { return now }, nil)
	tick.observe(read)
	reached := 0
	if _, err := tick.children(c.Spec.ID, func() ([]specdispatch.Child, error) {
		reached++
		return nil, nil
	}); err != nil {
		t.Fatalf("children: %v", err)
	}
	if reached != 1 {
		t.Errorf("a child set from an unknown schema answered without a read")
	}
}

// The tick's children wiring answers from the memory on a tick that finds the
// board unchanged, so the tick counts no bd call for it; a bead that moved
// reaches the town's read and is counted (gt-km9jt).
func TestSpecCandidateChildrenWiringSpendsItsReadsOnChangedBeads(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := specTestNow
	first := newSpecAnnotatedTickAt(root, "daemon-1", func() time.Time { return now }, nil)
	read := specCandidateBoardRead(t, specCandidateFixture()[:2])
	first.observe(read)
	for _, c := range read.Candidates {
		if _, err := first.children(c.Spec.ID, func() ([]specdispatch.Child, error) {
			return []specdispatch.Child{{ID: c.Spec.ID + ".1", Status: "closed"}}, nil
		}); err != nil {
			t.Fatalf("children %s: %v", c.Spec.ID, err)
		}
	}
	first.save()

	// A beacon bead with a statusless child would read as open, so its answer
	// proves the memory answered rather than a read this town cannot serve.
	reads := newSpecTickReads()
	reads.setPhase(specPhaseCandidates)
	second := newSpecAnnotatedTickAt(root, "daemon-1", func() time.Time { return now }, nil)
	second.observe(read)
	children := specCandidateChildren(t.TempDir(), reads, second)
	got, err := children(read.Candidates[0].Spec.ID)
	if err != nil {
		t.Fatalf("children from the memory: %v", err)
	}
	if len(got) != 1 || got[0].ID != read.Candidates[0].Spec.ID+".1" {
		t.Errorf("children = %+v, want the remembered read", got)
	}
	if calls := reads.endPhase(); calls != 0 {
		t.Errorf("the candidates phase counted %d calls for an unchanged candidate, want none", calls)
	}
}

// A bead the board no longer lists is dropped from the memory, so the file
// states what the dispatcher is considering rather than everything it ever saw
// (gt-km9jt).
func TestSpecCandidateChildrenForgetABeadThatLeavesTheBoard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tick := newSpecAnnotatedTickAt(root, "daemon-1", func() time.Time { return specTestNow }, nil)
	ids := func(read specBoardRead) []string {
		var out []string
		for _, c := range read.Candidates {
			out = append(out, c.Spec.ID)
		}
		return out
	}

	full := specCandidateBoardRead(t, specCandidateFixture()[:2])
	specCandidateChildrenTicks(t, tick, full)
	if got := len(loadSpecAnnotated(dispatch.SpecAnnotatedPath(root), "daemon-1").candidates); got != len(ids(full)) {
		t.Fatalf("memory holds %d candidates, want %d", got, len(ids(full)))
	}

	one := specCandidateBoardRead(t, specCandidateFixture()[:1])
	specCandidateChildrenTicks(t, newSpecAnnotatedTickAt(root, "daemon-1", func() time.Time { return specTestNow }, nil), one)

	kept := loadSpecAnnotated(dispatch.SpecAnnotatedPath(root), "daemon-1").candidates
	if _, ok := kept[ids(one)[0]]; !ok {
		t.Errorf("memory dropped the bead still on the board")
	}
	if _, ok := kept[ids(full)[1]]; ok {
		t.Errorf("memory still holds the bead the board no longer lists")
	}
}
