package land

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// skipByTitle is a FollowupSkip that drops the named findings with reason, the
// way the daemon's policy will (gt-3v8jno).
func skipByTitle(reason string, titles ...string) func(Work, FollowupCandidate) string {
	return func(_ Work, c FollowupCandidate) string {
		if slices.Contains(titles, c.Title) {
			return reason
		}
		return ""
	}
}

// TestFollowupSkipDropsTheNamedFinding: a finding FollowupSkip names a reason
// for is dropped before it reaches the bead, and the feed logs it (gt-3v8jno).
func TestFollowupSkipDropsTheNamedFinding(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{
		minorAt("internal/land/note.go", 230, "wording nit"),
		minorFinding("name the error"),
	})
	f.lander.FollowupSkip = skipByTitle("wording nit (Q34)", "wording nit")
	log := f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one for the finding the policy kept:\n%s", len(f.fake.created), log)
	}
	want := append([]string{FollowUpLabel, Fingerprint("internal/land/note.go", "name the error")}, BundleLabels(followupDay, "internal/land")...)
	if got := f.fake.created[0].Labels; !slices.Equal(got, want) {
		t.Errorf("labels = %v, want only the kept finding's fingerprint", got)
	}
	if strings.Contains(f.fake.created[0].Body, "wording nit") {
		t.Errorf("the bead body names the skipped finding:\n%s", f.fake.created[0].Body)
	}
	if !strings.Contains(log, "skipped 1 finding: wording nit (Q34)") {
		t.Errorf("log = %q, want the skip and its reason named", log)
	}
}

// TestFollowupSkipIsNeverQueuedOrCommented: a skipped finding is not work the
// feed holds for later, and it is not a repeat of a bead that carries it — the
// policy is asked before either decision (gt-3v8jno).
func TestFollowupSkipIsNeverQueuedOrCommented(t *testing.T) {
	t.Parallel()
	t.Run("not queued at the backpressure bound", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{
			minorAt("internal/land/note.go", 230, "wording nit a"),
			minorAt("internal/land/note.go", 231, "wording nit b"),
			minorFinding("name the error"),
		})
		f.lander.FollowupSkip = skipByTitle("wording nit (Q34)", "wording nit a", "wording nit b")
		f.fake.existing = openFollowupBeads(DefaultFollowupBackpressure)
		log := f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads at the backpressure bound", len(f.fake.created))
		}
		queued, err := f.reviews.Queued()
		if err != nil {
			t.Fatal(err)
		}
		if len(queued) != 1 || len(queued[0].Findings) != 1 || queued[0].Findings[0].Title != "name the error" {
			t.Errorf("queue lines = %+v, want one line holding only the finding the policy kept", queued)
		}
		// The line counts both skipped findings and names the reason once.
		if !strings.Contains(log, "skipped 2 findings: wording nit (Q34)") || strings.Count(log, "wording nit (Q34)") != 1 {
			t.Errorf("log = %q, want both findings counted and the reason once", log)
		}
	})
	t.Run("not commented onto the bead that carries it", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 230, "wording nit")})
		f.lander.FollowupSkip = skipByTitle("wording nit (Q34)", "wording nit")
		f.fake.existing = []ExistingFollowup{{
			ID: "gt-old", Title: "wording nit", Status: "open",
			Labels: []string{FollowUpLabel, Fingerprint("internal/land/note.go", "wording nit")},
		}}
		f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads for a skipped finding", len(f.fake.created))
		}
		comments, err := f.bd.Comments("gt-old")
		if err != nil {
			t.Fatal(err)
		}
		if len(comments) != 0 {
			t.Errorf("comments = %v, want none for a skipped finding", commentTexts(comments))
		}
	})
}

// panicOn is a FollowupSkip that panics on the named findings. The policy beads
// that will set the hook are exactly the code most likely to panic on an odd
// finding (gt-g97vlb).
func panicOn(value any, titles ...string) func(Work, FollowupCandidate) string {
	return func(_ Work, c FollowupCandidate) string {
		if slices.Contains(titles, c.Title) {
			panic(value)
		}
		return ""
	}
}

// TestFollowupSkipAPanickingHookKeepsTheFinding: a skip hook that panics keeps
// every finding rather than ending the landing worker's process, and its first
// panic reaches the feed's log once per landing, however many times the hook was
// asked (gt-g97vlb).
func TestFollowupSkipAPanickingHookKeepsTheFinding(t *testing.T) {
	t.Parallel()
	logged := func(t *testing.T, log, panicValue string) {
		t.Helper()
		want := fmt.Sprintf("the follow-up skip policy failed (%s); keeping the finding", panicValue)
		if n := strings.Count(log, "the follow-up skip policy failed"); n != 1 {
			t.Errorf("the log names the failed policy %d times, want once:\n%s", n, log)
		}
		if !strings.Contains(log, want) {
			t.Errorf("log = %q, want %q", log, want)
		}
	}
	t.Run("the landing's own finding", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{
			minorAt("internal/land/note.go", 230, "wording nit"),
			minorFinding("name the error"),
		})
		f.lander.FollowupSkip = panicOn("cannot read the finding", "wording nit")
		log := f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads, want one carrying both findings:\n%s", len(f.fake.created), log)
		}
		body := f.fake.created[0].Body
		for _, want := range []string{"wording nit", "name the error"} {
			if !strings.Contains(body, want) {
				t.Errorf("the filed bead lacks the kept finding %q:\n%s", want, body)
			}
		}
		logged(t, log, "cannot read the finding")
	})
	t.Run("a held queue line's finding", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.9, nil) // nothing of its own to file
		// The call number in the panic tells the one reported line from one per
		// call; the hook is asked once per finding in a pass (gt-r66hk7).
		calls := 0
		f.lander.FollowupSkip = func(Work, FollowupCandidate) string {
			calls++
			panic(fmt.Sprintf("cannot read the finding %d", calls))
		}
		if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
			t.Fatal(err)
		}
		f.bd.Seed(beads.Issue{ID: "gt-held", Title: "an earlier landing", Status: "closed", Type: "bug"})
		f.fake.existing = openFollowupBeads(1)
		log := f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads from the queue, want one for the kept finding:\n%s", len(f.fake.created), log)
		}
		if !strings.Contains(f.fake.created[0].Body, queuedFinding.Title) {
			t.Errorf("the filed bead lacks the kept finding:\n%s", f.fake.created[0].Body)
		}
		// The line is consumed only once its finding is filed.
		queued, err := f.reviews.Queued()
		if err != nil {
			t.Fatal(err)
		}
		if len(queued) != 0 {
			t.Errorf("queue = %+v, want the filed line consumed", queued)
		}
		if calls != 1 {
			t.Errorf("the hook was asked %d time(s), want once: the pass keeps the answer, a panic's included", calls)
		}
		logged(t, log, "cannot read the finding 1")
	})
}

// TestFollowupSkipDropsAQueuedFindingItNames: the hook reaches the queue too, so
// a queued finding it skips is not filed and its line is consumed rather than
// waiting forever (gt-3v8jno).
func TestFollowupSkipDropsAQueuedFindingItNames(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, nil)
	skip := skipByTitle("wording nit (Q34)", "name the error")
	var asked Work
	f.lander.FollowupSkip = func(w Work, c FollowupCandidate) string {
		asked = w
		return skip(w, c)
	}
	if err := f.reviews.AppendQueue(queueLine("gt-queued", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	log := f.run()
	if want := (Work{BeadID: "gt-queued", Rig: f.work.Rig}); asked != want {
		t.Errorf("the queued finding was asked with %+v, want only its bead and rig %+v", asked, want)
	}
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads for a queued finding the policy skipped:\n%s", len(f.fake.created), log)
	}
	queued, err := f.reviews.Queued()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Errorf("the log still holds %d queue lines, want the skipped one consumed", len(queued))
	}
	if !strings.Contains(log, "skipped 1 finding: wording nit (Q34)") {
		t.Errorf("log = %q, want the queued finding's skip named", log)
	}
}

// TestFollowupSkipQueuedWork: the hook is asked about a landing's own finding
// with the landing's whole Work, and about one read back from a queue line with
// only its bead and rig — the queue keeps no branch, head or target — and the
// FollowupSkip doc says so, because a policy that switches on a Work field reads
// it to know a queue line cannot carry one (gt-u7qgi0, om finding on gt-8fdomo).
func TestFollowupSkipQueuedWork(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/other.go", 40, "the fresh finding")})
	asked := map[string]Work{}
	f.lander.FollowupSkip = func(w Work, c FollowupCandidate) string {
		asked[c.Title] = w
		return ""
	}
	if err := f.reviews.AppendQueue(queueLine("gt-queued", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	f.run()
	wantQueued := Work{BeadID: "gt-queued", Rig: f.work.Rig}
	if got := asked[queuedFinding.Title]; got != wantQueued {
		t.Errorf("the queued finding was asked with %+v, want only the line's bead and rig %+v", got, wantQueued)
	}
	if got := asked["the fresh finding"]; got != f.work {
		t.Errorf("the landing's own finding was asked with %+v, want the landing's whole Work %+v", got, f.work)
	}
}

// followupSkipFieldDoc is the FollowupSkip field's doc comment in land.go, as
// its text reads.
func followupSkipFieldDoc(t *testing.T) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "land.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse land.go: %v", err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Lander" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					if name.Name == "FollowupSkip" && field.Doc != nil {
						return field.Doc.Text()
					}
				}
			}
		}
	}
	t.Fatal("land.go has no FollowupSkip field doc")
	return ""
}

// TestFollowupSkipFieldDocSaysPure: a held queue line is asked again on every
// later pass until it is consumed, so the hook must give the same answer each
// time. The field's doc says so, because that is where a policy's author learns
// the contract (gt-qlzw66).
func TestFollowupSkipFieldDocSaysPure(t *testing.T) {
	t.Parallel()
	doc := followupSkipFieldDoc(t)
	if !strings.Contains(doc, "must be pure") {
		t.Errorf("the FollowupSkip doc does not say it must be pure:\n%s", doc)
	}
}

// TestFollowupSkipIsAskedOncePerPassWithTheSameArguments: a queue line the feed
// cannot consume stays held, and the next feed pass asks the hook about its
// finding again. A pure hook has one answer to give, so the feed hands it the
// same Work and the same candidate on every pass (gt-qlzw66).
func TestFollowupSkipIsAskedOncePerPassWithTheSameArguments(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil) // nothing of its own to file
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	type ask struct {
		work      Work
		candidate FollowupCandidate
	}
	var asks []ask
	f.lander.FollowupSkip = func(w Work, c FollowupCandidate) string {
		asks = append(asks, ask{w, c})
		return ""
	}
	// The first pass cannot file, so the line is held rather than consumed and a
	// later pass asks about the same finding again.
	f.fake.createErr = errors.New("the beads store is unavailable")
	log := f.run()
	if len(asks) != 1 {
		t.Fatalf("the first pass asked the hook %d time(s), want once:\n%s", len(asks), log)
	}
	if queued, err := f.reviews.Queued(); err != nil || len(queued) != 1 {
		t.Fatalf("after the failed pass the queue holds %d line(s) (%v), want the line still held", len(queued), err)
	}
	first := asks[0]
	// The second pass files, so the line is consumed and dropped.
	f.fake.createErr = nil
	log = f.run()
	if len(asks) != 2 {
		t.Fatalf("the two passes asked the hook %d time(s), want once each:\n%s", len(asks), log)
	}
	if queued, err := f.reviews.Queued(); err != nil || len(queued) != 0 {
		t.Errorf("the queue holds %d line(s) (%v), want the filed line consumed", len(queued), err)
	}
	if asks[1] != first {
		t.Errorf("the second pass asked about %+v, want the first pass's %+v: the same finding must get the same arguments", asks[1], first)
	}
}

// TestFollowupSkipConsistentAcrossPasses: the drain and the compaction judge one
// queue line in the same feed pass, so a hook that would keep the finding on a
// second ask still leaves the line one outcome: skipped by the drain, consumed,
// dropped by the compaction, and asked once (gt-u7qgi0, om finding on gt-8fdomo).
func TestFollowupSkipConsistentAcrossPasses(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.9, nil) // nothing of its own to file
	if err := f.reviews.AppendQueue(queueLine("gt-held", f.work.Rig, queuedFinding)); err != nil {
		t.Fatal(err)
	}
	skip := &scriptedSkip{reply: func(_ string, n int) string {
		if n == 1 {
			return "wording nit (Q34)"
		}
		return "" // a second ask would keep the finding the first skipped
	}}
	f.lander.FollowupSkip = skip.ask
	log := f.run()
	if left, err := f.reviews.Queued(); err != nil || len(left) != 0 {
		t.Errorf("the queue holds %d line(s) (%v), want none: the compaction must drop the line the drain consumed", len(left), err)
	}
	if !strings.Contains(log, "dropped 1 consumed queue line(s)") {
		t.Errorf("log = %q, want the compaction's drop named", log)
	}
	if !strings.Contains(log, "skipped 1 finding: wording nit (Q34)") || skip.calls["name the error"] != 1 {
		t.Errorf("log = %q, asked = %v; want the skip named once and the hook asked once", log, skip.calls)
	}
}
