package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/events"
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

// untouchableStore fails the test on any Create or Update, and panics on any
// other method the embedded nil Client would be asked for, so a call that
// reaches the store before the refusal cannot pass unnoticed.
type untouchableStore struct {
	beads.Client
	t *testing.T
}

func (s untouchableStore) Create(beads.CreateOptions) (*beads.Issue, error) {
	s.t.Errorf("create reached the store")
	return &beads.Issue{ID: "gt-unexpected"}, nil
}

func (s untouchableStore) Update(id string, _ beads.UpdateOptions) error {
	s.t.Errorf("update of %s reached the store", id)
	return nil
}

func TestBeadUpdateRefusesBlankAcceptance(t *testing.T) {
	t.Parallel()
	for _, blank := range []string{"", "   ", "\n\t"} {
		blank := blank
		t.Run(fmt.Sprintf("%q", blank), func(t *testing.T) {
			t.Parallel()
			fake := beadsfake.New()
			is := mustCreateBead(t, fake, beads.CreateOptions{Title: "work"})
			criteria := "- [ ] unit tests pass"
			if err := fake.Update(is.ID, beads.UpdateOptions{Acceptance: &criteria}); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			v := &beadVerbs{client: untouchableStore{Client: fake, t: t}, actor: "me", out: &out}
			err := v.update(is.ID, beads.UpdateOptions{Acceptance: &blank})
			if err == nil {
				t.Fatal("a blank acceptance succeeded")
			}
			if !strings.Contains(err.Error(), "--acceptance") {
				t.Errorf("error %q does not name --acceptance", err)
			}
			if !strings.Contains(err.Error(), "not changed") {
				t.Errorf("error %q does not say the criteria were not changed", err)
			}
			if got := mustShowBead(t, fake, is.ID).AcceptanceCriteria; got != criteria {
				t.Errorf("acceptance criteria = %q, want %q kept", got, criteria)
			}
		})
	}
}

func TestBeadCreateRefusesBlankAcceptance(t *testing.T) {
	t.Parallel()
	for _, blank := range []string{"", "   ", "\n\t"} {
		blank := blank
		t.Run(fmt.Sprintf("%q", blank), func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			v := &beadVerbs{client: untouchableStore{t: t}, out: &out}
			err := v.create(beadCreateRequest{title: "work", acceptance: blank, acceptanceSet: true})
			if err == nil {
				t.Fatal("a blank acceptance created a bead")
			}
			if !strings.Contains(err.Error(), "--acceptance") {
				t.Errorf("error %q does not name --acceptance", err)
			}
		})
	}

	// A create that omits the flag is not a request for a blank block.
	t.Run("omitted", func(t *testing.T) {
		t.Parallel()
		v, fake, out := testBeadVerbs("me")
		if err := v.create(beadCreateRequest{title: "work", quiet: true}); err != nil {
			t.Fatal(err)
		}
		id := strings.TrimSpace(out.String())
		if got := mustShowBead(t, fake, id).AcceptanceCriteria; got != "" {
			t.Errorf("acceptance criteria = %q, want none", got)
		}
	})
}

// TestBeadFlagMapping covers the mapping each write verb does before it
// reaches a store. An omitted --acceptance must stay nil so update leaves the
// criteria alone, and an explicit blank one must arrive non-nil so the verb
// refuses it (gt-aht2p). The flags come from the same registration the
// commands use, so the test cannot drift from them.
func TestBeadFlagMapping(t *testing.T) {
	t.Parallel()
	newUpdateFlags := func() *pflag.FlagSet {
		f := pflag.NewFlagSet("update", pflag.ContinueOnError)
		beadUpdateFlags(f)
		return f
	}
	t.Run("update omits acceptance", func(t *testing.T) {
		if got := beadUpdateOptions(newUpdateFlags()).Acceptance; got != nil {
			t.Errorf("omitted --acceptance mapped to %q, want nil", *got)
		}
	})
	t.Run("update keeps a blank acceptance for the verb to refuse", func(t *testing.T) {
		f := newUpdateFlags()
		if err := f.Set("acceptance", ""); err != nil {
			t.Fatal(err)
		}
		got := beadUpdateOptions(f).Acceptance
		if got == nil || *got != "" {
			t.Fatalf("--acceptance= mapped to %v, want a pointer to the blank value", got)
		}
	})
	t.Run("update maps given values", func(t *testing.T) {
		f := newUpdateFlags()
		given := map[string]string{
			"title": "DISPROVEN: x", "priority": "1", "acceptance": "- [ ] x",
			"add-label": "l1", "remove-label": "l2",
		}
		for flag, value := range given {
			if err := f.Set(flag, value); err != nil {
				t.Fatal(err)
			}
		}
		opts := beadUpdateOptions(f)
		if opts.Title == nil || *opts.Title != given["title"] {
			t.Errorf("title = %v, want %q", opts.Title, given["title"])
		}
		if opts.Priority == nil || *opts.Priority != 1 {
			t.Errorf("priority = %v, want 1", opts.Priority)
		}
		if opts.Acceptance == nil || *opts.Acceptance != given["acceptance"] {
			t.Errorf("acceptance = %v, want %q", opts.Acceptance, given["acceptance"])
		}
		if !reflect.DeepEqual(opts.AddLabels, []string{"l1"}) || !reflect.DeepEqual(opts.RemoveLabels, []string{"l2"}) {
			t.Errorf("labels to add %v, remove %v", opts.AddLabels, opts.RemoveLabels)
		}
	})
	t.Run("update passes metadata pairs through to bd", func(t *testing.T) {
		f := newUpdateFlags()
		for _, pair := range []string{"spec_dispatch_timeout_attempts=2", "other=a=b"} {
			if err := f.Set("set-metadata", pair); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.Set("unset-metadata", "stale"); err != nil {
			t.Fatal(err)
		}
		opts := beadUpdateOptions(f)
		// The pair is bd's to read: a value holding '=' must reach it whole
		// rather than being split here (gt-vaivp).
		want := []string{"spec_dispatch_timeout_attempts=2", "other=a=b"}
		if !reflect.DeepEqual(opts.SetMetadata, want) {
			t.Errorf("metadata set = %v, want %v", opts.SetMetadata, want)
		}
		if !reflect.DeepEqual(opts.UnsetMetadata, []string{"stale"}) {
			t.Errorf("metadata unset = %v, want [stale]", opts.UnsetMetadata)
		}
	})

	newCreateFlags := func() *pflag.FlagSet {
		f := pflag.NewFlagSet("create", pflag.ContinueOnError)
		beadCreateFlags(f)
		return f
	}
	t.Run("create omits acceptance", func(t *testing.T) {
		r, err := beadCreateRequestFrom(newCreateFlags(), []string{"work"})
		if err != nil {
			t.Fatal(err)
		}
		if r.acceptanceSet {
			t.Error("omitted --acceptance mapped as set")
		}
	})
	t.Run("create marks an explicit blank acceptance", func(t *testing.T) {
		f := newCreateFlags()
		if err := f.Set("acceptance", ""); err != nil {
			t.Fatal(err)
		}
		r, err := beadCreateRequestFrom(f, []string{"work"})
		if err != nil {
			t.Fatal(err)
		}
		if !r.acceptanceSet {
			t.Error("--acceptance= mapped as unset, so the store would drop it")
		}
	})
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

// TestBeadCloseEndsTheAttachedMolecule: a work bead closed through this verb
// takes its molecule with it, steps first. The polecat that closes its own bead
// to report no-changes (mol-deep-review's submit step) then exits without a
// landing, so nothing else would end the molecule: gt done's close block needs a
// COMPLETED or ESCALATED exit and a landed bead's molecule belongs to the
// landing (gt-x1d8s).
func TestBeadCloseEndsTheAttachedMolecule(t *testing.T) {
	t.Parallel()
	v, fake, _ := testBeadVerbs("gastown/polecats/opal")
	fake.Seed(beads.Issue{ID: "gt-work-1", Title: "Work", Status: string(beads.StatusHooked),
		Description: "attached_molecule: gt-wisp-mol1\nattached_formula: mol-deep-review"})
	fake.Seed(beads.Issue{ID: "gt-wisp-mol1", Title: "mol-deep-review", Status: "open", Ephemeral: true})
	seedChildren(t, fake, "gt-wisp-mol1",
		beads.Issue{ID: "gt-step-1", Title: "Step 1", Status: "open", Ephemeral: true},
		beads.Issue{ID: "gt-step-2", Title: "Step 2", Status: "open", Ephemeral: true})

	if err := v.close("no-changes: report-only deep review", "gt-work-1"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"gt-step-1", "gt-step-2", "gt-wisp-mol1"} {
		if got := mustShowBead(t, fake, id); got.Status != "closed" {
			t.Errorf("%s = %s, want closed", id, got.Status)
		}
	}
	if got := mustShowBead(t, fake, "gt-work-1"); got.CloseReason != "no-changes: report-only deep review" {
		t.Errorf("work bead close reason = %q, want the caller's", got.CloseReason)
	}
}

// TestBeadCloseWithoutAttachmentLeavesMoleculesAlone: a bead carrying no
// attachment closes exactly as before — no molecule is guessed at.
func TestBeadCloseWithoutAttachmentLeavesMoleculesAlone(t *testing.T) {
	t.Parallel()
	v, fake, _ := testBeadVerbs("gastown/polecats/opal")
	plain := mustCreateBead(t, fake, beads.CreateOptions{Title: "plain"})
	mol := mustCreateBead(t, fake, beads.CreateOptions{Title: "mol-polecat-work"})

	if err := v.close("sub-issue done", plain.ID); err != nil {
		t.Fatal(err)
	}

	if got := mustShowBead(t, fake, plain.ID); got.Status != "closed" {
		t.Errorf("%s = %s, want closed", plain.ID, got.Status)
	}
	if got := mustShowBead(t, fake, mol.ID); got.Status == "closed" {
		t.Errorf("molecule %s was closed by a close of %s", mol.ID, plain.ID)
	}
}

// beadCloseTestVerbs is testBeadVerbs with the store's own actor set to the
// same actor, so the fake's close refuses an assignee that is not it — bd's
// rule, and where a half-refused batch comes from in a test.
func beadCloseTestVerbs(actor string) (*beadVerbs, *beadsfake.Fake, *bytes.Buffer) {
	fake := beadsfake.New(beadsfake.WithActor(actor))
	var out bytes.Buffer
	return &beadVerbs{
		client:       fake,
		actor:        actor,
		out:          &out,
		closeRefusal: func(string, string) string { return "" },
	}, fake, &out
}

// showFailingStore is a store whose Show fails for one bead and delegates the
// rest, so a case can break the read endMoleculesOf makes of a bead it is
// about to end without breaking every other read.
type showFailingStore struct {
	beads.Client
	failID string
	err    error
}

func (s showFailingStore) Show(id string) (*beads.Issue, error) {
	if id == s.failID {
		return nil, s.err
	}
	return s.Client.Show(id)
}

// TestBeadClosePartialEndsOnlyClosedMolecules covers the reading endMoleculesOf
// makes of a batch close's result: beads.ClosedIDs names only the beads that
// actually closed, and only those have a molecule the close can be said to have
// ended. A bead bd refused is still open work — its molecule is what a later
// close of that bead ends, and ending it here would strand a workflow whose
// bead never closed. A bead whose own read fails while its molecule is being
// ended is a warning, not a failed close: the close the caller asked for had
// already happened (gt-7cmjp).
func TestBeadClosePartialEndsOnlyClosedMolecules(t *testing.T) {
	t.Parallel()
	v, fake, _ := beadCloseTestVerbs("gastown/polecats/opal")
	for _, suffix := range []string{"a", "b"} {
		assignee := ""
		if suffix == "b" {
			// Another actor's bead: bd skips it in the batch and closes the rest.
			assignee = "gastown/polecats/furiosa"
		}
		fake.Seed(beads.Issue{ID: "gt-work-" + suffix, Title: "Work " + suffix, Status: string(beads.StatusHooked),
			Assignee: assignee, Description: "attached_molecule: gt-wisp-" + suffix})
		fake.Seed(beads.Issue{ID: "gt-wisp-" + suffix, Title: "mol-polecat-work", Status: "open", Ephemeral: true})
		seedChildren(t, fake, "gt-wisp-"+suffix,
			beads.Issue{ID: "gt-step-" + suffix, Title: "Step", Status: "open", Ephemeral: true})
	}

	err := v.close("", "gt-work-a", "gt-work-b")
	if err == nil {
		t.Fatal("closing a batch bd half-refused reported success")
	}
	var pe *beads.PartialCloseError
	if !errors.As(err, &pe) || !reflect.DeepEqual(pe.NotClosed, []string{"gt-work-b"}) {
		t.Fatalf("close error = %v, want the batch's own partial-close error, naming gt-work-b", err)
	}
	for _, id := range []string{"gt-step-a", "gt-wisp-a"} {
		if got := mustShowBead(t, fake, id); got.Status != "closed" {
			t.Errorf("%s = %s, want closed: gt-work-a closed, so its workflow ended with it", id, got.Status)
		}
	}
	for _, id := range []string{"gt-step-b", "gt-wisp-b"} {
		if got := mustShowBead(t, fake, id); got.Status == "closed" {
			t.Errorf("%s was closed by a close that left gt-work-b open", id)
		}
	}
	if got := mustShowBead(t, fake, "gt-work-b"); got.Status == "closed" {
		t.Errorf("gt-work-b = %s, want it still open: the refusal is what leaves its molecule for a later close", got.Status)
	}

	// The read endMoleculesOf makes of a closed bead can fail on its own. The
	// close stands — it is reported to the caller as the success it was — and
	// the molecule is left for whoever reaps closed wisps.
	fake.Seed(beads.Issue{ID: "gt-work-c", Title: "Work c", Status: string(beads.StatusHooked),
		Description: "attached_molecule: gt-wisp-c"})
	fake.Seed(beads.Issue{ID: "gt-wisp-c", Title: "mol-polecat-work", Status: "open", Ephemeral: true})
	v.client = showFailingStore{Client: fake, failID: "gt-work-c", err: errors.New("database is locked")}
	if err := v.close("", "gt-work-c"); err != nil {
		t.Errorf("close of gt-work-c = %v, want nil: its molecule's read failing is not the close's failure", err)
	}
	if got := mustShowBead(t, fake, "gt-work-c"); got.Status != "closed" {
		t.Errorf("gt-work-c = %s, want closed", got.Status)
	}
	if got := mustShowBead(t, fake, "gt-wisp-c"); got.Status != "open" {
		t.Errorf("gt-wisp-c = %s, want open: its bead's read failed, so nothing ended it", got.Status)
	}
}

// Closing a bead ends its run on the town's clock: a bead closed by hand and
// dispatched again later must start over, not be dated from the dispatch of the
// run that was closed (gt-tzbtf). Only the beads the close actually closed are
// recorded.
func TestBeadCloseRecordsTheRunEnd(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	v, fake, _ := testBeadVerbs("gastown/polecats/opal")
	var recorded []string
	v.recordClosed = func(ids []string) { recorded = append(recorded, ids...); logClosedBeads(town, v.actor, ids) }
	a := mustCreateBead(t, fake, beads.CreateOptions{Title: "a"})
	b := mustCreateBead(t, fake, beads.CreateOptions{Title: "b"})
	// A close the invariant refuses writes nothing, so it ends no run either.
	v.closeRefusal = func(id, _ string) string {
		if id == b.ID {
			return "branch carries 2 unmerged commits"
		}
		return ""
	}
	if err := v.close("done", a.ID, b.ID); err == nil {
		t.Fatal("close past the invariant succeeded")
	}
	if len(recorded) != 0 {
		t.Errorf("refused close recorded run ends %v, want none", recorded)
	}

	v.closeRefusal = func(string, string) string { return "" }
	if err := v.close("done", a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if want := []string{a.ID, b.ID}; !reflect.DeepEqual(recorded, want) {
		t.Errorf("recorded run ends %v, want %v", recorded, want)
	}

	// The record the close wrote is the one the dispatch clock reads.
	evs, err := events.Read(town)
	if err != nil {
		t.Fatal(err)
	}
	var written []string
	for _, ev := range evs {
		if ev.Type != events.TypeBeadClosed {
			continue
		}
		bead, _ := ev.Payload["bead"].(string)
		written = append(written, bead)
	}
	if want := []string{a.ID, b.ID}; !reflect.DeepEqual(written, want) {
		t.Fatalf("bead_closed events = %v, want %v", written, want)
	}
}
