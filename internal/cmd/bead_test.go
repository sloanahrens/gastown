package cmd

import (
	"bytes"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// testBeadVerbs returns verbs acting as actor on a fresh fake database,
// allowing every close, and the buffer they report to.
func testBeadVerbs(actor string) (*beadVerbs, *beadsfake.Fake, *bytes.Buffer) {
	fake := beadsfake.New()
	var out bytes.Buffer
	return &beadVerbs{
		client:       fake,
		actor:        actor,
		out:          &out,
		closeRefusal: func(string, string) string { return "" },
	}, fake, &out
}

func mustCreateBead(t *testing.T, c beads.Client, opts beads.CreateOptions) *beads.Issue {
	t.Helper()
	opts.Priority = -1
	is, err := c.Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	return is
}

func mustShowBead(t *testing.T, c beads.Client, id string) *beads.Issue {
	t.Helper()
	is, err := c.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	return is
}

func TestBeadCreate(t *testing.T) {
	t.Parallel()
	v, fake, out := testBeadVerbs("gastown/polecats/opal")
	criteria := "- [ ] the flag sets the field"
	err := v.create(beadCreateRequest{title: "Found: nil map", kind: "bug", priority: 1,
		labels: []string{"pr-review"}, description: "details", acceptance: criteria, quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(out.String())
	got := mustShowBead(t, fake, id)
	sort.Strings(got.Labels)
	if got.Title != "Found: nil map" || got.Priority != 1 || got.Description != "details" ||
		got.AcceptanceCriteria != criteria ||
		got.CreatedBy != "gastown/polecats/opal" || !reflect.DeepEqual(got.Labels, []string{"gt:bug", "pr-review"}) {
		t.Errorf("created %+v", got)
	}

	if err := v.create(beadCreateRequest{title: "  ", priority: -1}); err == nil {
		t.Error("create with a blank title succeeded")
	}
	if err := v.create(beadCreateRequest{title: "--help", priority: -1}); err == nil {
		t.Error("create with a flag-like title succeeded")
	}
}

func TestBeadNoteAndUpdate(t *testing.T) {
	t.Parallel()
	v, fake, _ := testBeadVerbs("me")
	is := mustCreateBead(t, fake, beads.CreateOptions{Title: "work"})
	if err := v.note(is.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if err := v.note(is.ID, "second"); err != nil {
		t.Fatal(err)
	}
	if err := v.note(is.ID, " "); err == nil {
		t.Error("a blank note succeeded")
	}
	title, prio := "DISPROVEN: x", 1
	if err := v.update(is.ID, beads.UpdateOptions{Title: &title, Priority: &prio, AddLabels: []string{"l1"}}); err != nil {
		t.Fatal(err)
	}
	got := mustShowBead(t, fake, is.ID)
	if got.Notes != "first\nsecond" || got.Title != title || got.Priority != 1 || !reflect.DeepEqual(got.Labels, []string{"l1"}) {
		t.Errorf("after note and update: notes %q title %q priority %d labels %v", got.Notes, got.Title, got.Priority, got.Labels)
	}
	// gt-n623a: ticking a box has no other route, and a wrong block would
	// leave the landing worker the sole judge.
	criteria := "- [x] gt done refuses\n- [ ] unit tests pass"
	if err := v.update(is.ID, beads.UpdateOptions{Acceptance: &criteria}); err != nil {
		t.Fatal(err)
	}
	if got := mustShowBead(t, fake, is.ID).AcceptanceCriteria; got != criteria {
		t.Errorf("acceptance criteria = %q, want %q", got, criteria)
	}
	if err := v.update(is.ID, beads.UpdateOptions{}); err == nil {
		t.Error("an empty update succeeded")
	}
}

func TestBeadClaim(t *testing.T) {
	t.Parallel()
	v, fake, _ := testBeadVerbs("me")
	free := mustCreateBead(t, fake, beads.CreateOptions{Title: "free"})
	if err := v.claim(free.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustShowBead(t, fake, free.ID); got.Status != "in_progress" || got.Assignee != "me" {
		t.Errorf("claimed bead is %s/%q", got.Status, got.Assignee)
	}
	if err := v.claim(free.ID); err != nil {
		t.Errorf("reclaiming my own bead: %v", err)
	}

	hooked := mustCreateBead(t, fake, beads.CreateOptions{Title: "slung to me"})
	hookedStatus, me := "hooked", "me"
	if err := fake.Update(hooked.ID, beads.UpdateOptions{Status: &hookedStatus, Assignee: &me}); err != nil {
		t.Fatal(err)
	}
	if err := v.claim(hooked.ID); err != nil {
		t.Fatal(err)
	}
	if st := mustShowBead(t, fake, hooked.ID).Status; st != "in_progress" {
		t.Errorf("claimed hooked bead is %s", st)
	}

	theirs := mustCreateBead(t, fake, beads.CreateOptions{Title: "theirs"})
	other := "other"
	if err := fake.Update(theirs.ID, beads.UpdateOptions{Assignee: &other}); err != nil {
		t.Fatal(err)
	}
	if err := v.claim(theirs.ID); err == nil || !strings.Contains(err.Error(), "other holds it") {
		t.Errorf("claiming another agent's bead: %v", err)
	}
	if a := mustShowBead(t, fake, theirs.ID).Assignee; a != "other" {
		t.Errorf("refused claim moved the assignee to %q", a)
	}

	nobody, _, _ := testBeadVerbs("")
	nobody.client = fake
	if err := nobody.claim(free.ID); err == nil {
		t.Error("claim with no actor succeeded")
	}
}

func TestBeadReset(t *testing.T) {
	t.Parallel()
	v, fake, _ := testBeadVerbs("deacon")
	orphan := mustCreateBead(t, fake, beads.CreateOptions{Title: "orphan"})
	inProgress, dead := "in_progress", "gastown/polecats/dead"
	if err := fake.Update(orphan.ID, beads.UpdateOptions{Status: &inProgress, Assignee: &dead}); err != nil {
		t.Fatal(err)
	}
	if err := fake.AppendNotes(orphan.ID, "earlier"); err != nil {
		t.Fatal(err)
	}
	if err := v.reset(orphan.ID, "Orphaned with partial work on b"); err != nil {
		t.Fatal(err)
	}
	got := mustShowBead(t, fake, orphan.ID)
	if got.Status != "open" || got.Assignee != "" || got.Notes != "earlier\nOrphaned with partial work on b" {
		t.Errorf("reset bead: %s/%q notes %q", got.Status, got.Assignee, got.Notes)
	}

	landing := mustCreateBead(t, fake, beads.CreateOptions{Title: "submitted", Labels: []string{land.LabelReadyToLand}})
	if err := fake.Update(landing.ID, beads.UpdateOptions{Status: &inProgress, Assignee: &dead}); err != nil {
		t.Fatal(err)
	}
	if err := v.reset(landing.ID, ""); err == nil || !strings.Contains(err.Error(), land.LabelReadyToLand) {
		t.Errorf("resetting ready-to-land work: %v", err)
	}
	if a := mustShowBead(t, fake, landing.ID).Assignee; a != dead {
		t.Errorf("refused reset cleared the assignee to %q", a)
	}
}

func TestBeadCommentAndDeps(t *testing.T) {
	t.Parallel()
	v, fake, _ := testBeadVerbs("me")
	a := mustCreateBead(t, fake, beads.CreateOptions{Title: "a"})
	b := mustCreateBead(t, fake, beads.CreateOptions{Title: "b"})
	if err := v.comment(a.ID, "PR-SHERIFF-EVIDENCE: done"); err != nil {
		t.Fatal(err)
	}
	cs, err := fake.Comments(a.ID)
	if err != nil || len(cs) != 1 || cs[0].Text != "PR-SHERIFF-EVIDENCE: done" {
		t.Errorf("comments = %+v, %v", cs, err)
	}
	if err := v.depAdd(a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if deps := mustShowBead(t, fake, a.ID).Dependencies; len(deps) != 1 || deps[0].ID != b.ID {
		t.Errorf("after dep add, %s depends on %+v", a.ID, deps)
	}
	if err := v.depRemove(a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if deps := mustShowBead(t, fake, a.ID).Dependencies; len(deps) != 0 {
		t.Errorf("after dep remove, %s depends on %v", a.ID, deps)
	}
	if err := v.depAdd(a.ID, a.ID); err == nil {
		t.Error("a self-dependency succeeded")
	}
}

func TestBeadCloseAndReopen(t *testing.T) {
	t.Parallel()
	v, fake, _ := testBeadVerbs("me")
	a := mustCreateBead(t, fake, beads.CreateOptions{Title: "a"})
	b := mustCreateBead(t, fake, beads.CreateOptions{Title: "b"})
	if err := v.close("no-changes: already fixed", a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if got := mustShowBead(t, fake, id); got.Status != "closed" || got.CloseReason != "no-changes: already fixed" {
			t.Errorf("%s: %s %q", id, got.Status, got.CloseReason)
		}
	}
	if err := v.reopen(a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if st := mustShowBead(t, fake, id).Status; st != "open" {
			t.Errorf("%s reopened to %s", id, st)
		}
	}

	var judged []string
	v.closeRefusal = func(id, reason string) string {
		judged = append(judged, id+"|"+reason)
		if id == b.ID {
			return "branch carries 2 unmerged commits"
		}
		return ""
	}
	err := v.close("done", a.ID, b.ID)
	if err == nil || !strings.Contains(err.Error(), "2 unmerged commits") {
		t.Fatalf("close past the invariant: %v", err)
	}
	if !reflect.DeepEqual(judged, []string{a.ID + "|done", b.ID + "|done"}) {
		t.Errorf("invariant judged %v", judged)
	}
	if st := mustShowBead(t, fake, a.ID).Status; st != "open" {
		t.Errorf("a refused batch still closed %s", a.ID)
	}
}
