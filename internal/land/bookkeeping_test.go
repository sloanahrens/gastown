package land

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestBookkeepingRecordDurable: recordDurable writes the durable half of a
// landing record — refs/landed/<bead>, then the rig's landings file — and
// leaves the work bead to recordBead. A failed ref write returns before the
// landings line, and a failed append returns the error with the ref already
// written. Every case leaves the bead ready to land, because the bead write is
// the step that follows recordDurable.
func TestBookkeepingRecordDurable(t *testing.T) {
	t.Parallel()

	durable := func(t *testing.T, f *landFixture, l *Lander, g Repo) (LandingRecord, error) {
		t.Helper()
		return l.recordDurable(g, f.work, Result{Base: f.base, LandedCommit: f.work.Head})
	}
	// records is the landings file's non-blank lines: a file left empty reads
	// as no records, where landingLines reports its one empty line.
	records := func(f *landFixture) []string {
		var out []string
		for _, line := range f.landingLines() {
			if strings.TrimSpace(line) != "" {
				out = append(out, line)
			}
		}
		return out
	}
	// landed: both repositories hold the ref and the landings file holds the
	// one record.
	landed := func(t *testing.T, f *landFixture) {
		t.Helper()
		for _, dir := range []string{f.repo, f.origin} {
			if got := f.refAt(dir, LandedRef(f.work.BeadID)); got != f.work.Head {
				t.Errorf("%s in %s = %q, want the landed head %q", LandedRef(f.work.BeadID), dir, got, f.work.Head)
			}
		}
		lines := records(f)
		if len(lines) != 1 {
			t.Fatalf("landings file holds %d records; want 1", len(lines))
		}
		var rec LandingRecord
		if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
			t.Fatalf("decoding the landing record: %v", err)
		}
		if rec.BeadID != f.work.BeadID || rec.Head != f.work.Head || rec.LandedCommit != f.work.Head {
			t.Errorf("landings record = %s at head %s landed %s; want %s at %s",
				rec.BeadID, rec.Head, rec.LandedCommit, f.work.BeadID, f.work.Head)
		}
	}
	// readyToLand: the bead recordDurable did not touch is still waiting, with
	// no landing note on it.
	readyToLand := func(t *testing.T, f *landFixture) {
		t.Helper()
		if b := f.bead(); b.Status == "closed" || strings.Contains(b.Notes, LandingNoteMarker) {
			t.Errorf("the bead was written without recordBead: status=%s notes=%q", b.Status, b.Notes)
		}
	}

	t.Run("writes the ref then the landings line", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		rec, err := durable(t, f, f.lander(), f.git.Open(f.repo))
		if err != nil {
			t.Fatalf("recordDurable: %v", err)
		}
		if rec.BeadID != f.work.BeadID || rec.Head != f.work.Head {
			t.Errorf("recordDurable returned %s at %s; want %s at %s",
				rec.BeadID, rec.Head, f.work.BeadID, f.work.Head)
		}
		landed(t, f)
		readyToLand(t, f)
	})

	t.Run("a failed ref write stops before the landings file", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		g := landedRefRepo{Repo: f.git.Open(f.repo), failUpdate: true}

		rec, err := durable(t, f, f.lander(), g)
		if err == nil || !strings.Contains(err.Error(), "setting "+LandedRef(f.work.BeadID)) {
			t.Fatalf("recordDurable = %v, want the failed ref write", err)
		}
		if rec.BeadID != f.work.BeadID {
			t.Errorf("recordDurable returned %+v; want the record it built", rec)
		}
		if n := len(records(f)); n != 0 {
			t.Errorf("landings records = %d, want none: the file follows the ref", n)
		}
		if got := f.refAt(f.repo, LandedRef(f.work.BeadID)); got != "" {
			t.Errorf("%s = %q after a refused update; want unset", LandedRef(f.work.BeadID), got)
		}
		readyToLand(t, f)
	})

	t.Run("a failed append returns the error with the ref written", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		l := f.lander()
		lf, err := RigLandingsFile(f.town, "gastown")
		if err != nil {
			t.Fatal(err)
		}
		lf.write = func(*os.File, []byte) (int, error) { return 0, errors.New("disk full") }
		l.Landings = lf

		rec, err := durable(t, f, l, f.git.Open(f.repo))
		if err == nil || !strings.Contains(err.Error(), "landings file:") {
			t.Fatalf("recordDurable = %v, want the failed append", err)
		}
		if rec.BeadID != f.work.BeadID {
			t.Errorf("recordDurable returned %+v; want the record it built", rec)
		}
		if got := f.refAt(f.repo, LandedRef(f.work.BeadID)); got != f.work.Head {
			t.Errorf("%s = %q after the append failed; want the head %q", LandedRef(f.work.BeadID), got, f.work.Head)
		}
		if n := len(records(f)); n != 0 {
			t.Errorf("landings records = %d, want none: the append failed", n)
		}
		readyToLand(t, f)
	})
}

// TestBookkeepingDeferred: DeferBookkeeping ends Land after the durable half of
// the record — the landed ref and the landings line — and hands the record back
// in Result.Deferred, leaving the bead open and un-noted and the follow-up feed
// unrun. The same landing with the flag off records both halves in one pass and
// files the follow-up its approved review earned (gt-i4kqoa).
func TestBookkeepingDeferred(t *testing.T) {
	t.Parallel()

	// land runs the fixture's landing with the flag as given and a review that
	// approves with one minor finding, so a follow-up feed that ran is visible.
	land := func(t *testing.T, deferBookkeeping bool) (*landFixture, *followupFake, Result) {
		t.Helper()
		f := newLandFixture(t)
		l := f.lander()
		l.DeferBookkeeping = deferBookkeeping
		followups := &followupFake{}
		l.Followups = followups
		f.review.fn = func(string) (Verdict, error) {
			return Verdict{Verdict: VerdictApprove, Score: 0.9,
				Findings: []Finding{minorFinding("name the error")}}, nil
		}
		res, err := l.Land(context.Background(), f.work)
		if err != nil {
			t.Fatalf("Land: %v", err)
		}
		return f, followups, res
	}
	// durable reads the half of the record the landing wrote before any bead
	// write: the ref in the lander's clone and the one landings line, decoded.
	durable := func(t *testing.T, f *landFixture) LandingRecord {
		t.Helper()
		if got := f.refAt(f.repo, LandedRef(f.work.BeadID)); got != f.work.Head {
			t.Errorf("%s in the lander's clone = %q, want the landed head %q", LandedRef(f.work.BeadID), got, f.work.Head)
		}
		lines := f.landingLines()
		if len(lines) != 1 {
			t.Fatalf("landings lines = %q, want one", lines)
		}
		var rec LandingRecord
		if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
			t.Fatalf("decoding the landing record: %v", err)
		}
		return rec
	}

	t.Run("the flag on returns the record and leaves the bead to a later pass", func(t *testing.T) {
		t.Parallel()
		f, followups, res := land(t, true)
		rec := durable(t, f)
		if rec.BeadID != f.work.BeadID || rec.Head != f.work.Head || rec.LandedCommit != res.LandedCommit {
			t.Errorf("landings record = %s at %s landed %s; want %s at %s landed %s",
				rec.BeadID, rec.Head, rec.LandedCommit, f.work.BeadID, f.work.Head, res.LandedCommit)
		}
		if res.Deferred == nil {
			t.Fatal("Result.Deferred is nil; want the record the durable half wrote")
		}
		if got := *res.Deferred; got.BeadID != rec.BeadID || got.Head != rec.Head || got.LandedCommit != rec.LandedCommit ||
			got.PatchID != rec.PatchID || !got.LandedAt.Equal(rec.LandedAt) {
			t.Errorf("Result.Deferred = %+v, want the landings record %+v", got, rec)
		}
		b := f.bead()
		if b.Status == "closed" || !beads.HasLabel(b, LabelReadyToLand) || strings.Contains(b.Notes, LandingNoteMarker) {
			t.Errorf("the bead was written during a deferred landing: status=%s labels=%v notes=%q", b.Status, b.Labels, b.Notes)
		}
		if n := len(followups.created); n != 0 {
			t.Errorf("the deferred landing filed %d follow-up bead(s); want none before the bead half", n)
		}
	})

	t.Run("the flag off records and files in one pass", func(t *testing.T) {
		t.Parallel()
		f, followups, res := land(t, false)
		durable(t, f) // the durable half is written on this path too
		if res.Deferred != nil {
			t.Errorf("Result.Deferred = %+v on a landing that was not deferred; want nil", *res.Deferred)
		}
		b := f.bead()
		if b.Status != "closed" || beads.HasLabel(b, LabelReadyToLand) || !strings.Contains(b.Notes, LandingNoteMarker) {
			t.Errorf("bead status=%s labels=%v notes=%q; want the landing recorded and the bead closed", b.Status, b.Labels, b.Notes)
		}
		if n := len(followups.created); n != 1 {
			t.Errorf("filed %d follow-up bead(s) for an approved landing with one minor; want 1", n)
		}
	})

	t.Run("the flag on with a failed landings append leaves Deferred nil", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		l := f.lander()
		l.DeferBookkeeping = true
		followups := &followupFake{}
		l.Followups = followups
		lf, err := RigLandingsFile(f.town, "gastown")
		if err != nil {
			t.Fatal(err)
		}
		lf.write = func(*os.File, []byte) (int, error) { return 0, errors.New("disk full") }
		l.Landings = lf

		res, err := l.Land(context.Background(), f.work)
		var recErr *RecordError
		if !errors.As(err, &recErr) {
			t.Fatalf("Land = %v, want a *RecordError from the failed durable write", err)
		}
		if res.Deferred != nil {
			t.Errorf("Result.Deferred = %+v after the durable write failed; want nil", *res.Deferred)
		}
		b := f.bead()
		if b.Status == "closed" || !beads.HasLabel(b, LabelReadyToLand) || strings.Contains(b.Notes, LandingNoteMarker) {
			t.Errorf("the bead was written after the durable write failed: status=%s labels=%v notes=%q", b.Status, b.Labels, b.Notes)
		}
		if n := len(followups.created); n != 0 {
			t.Errorf("filed %d follow-up bead(s) after the durable write failed; want none", n)
		}
	})
}

// updateFails is a beads store whose reads answer and whose update refuses, as
// a Dolt write outage does: the record's write fails while everything the feed
// reads through the same store still answers.
type updateFails struct{ Beads }

func (updateFails) Update(string, beads.UpdateOptions) error { return errors.New("dolt write refused") }

// TestBookkeepingFinish: FinishLanding writes the half of a deferred landing's
// record the landing left for a later pass — the bead's LANDING RECORD block,
// the ready label off and the close — and only then runs the follow-up feed the
// approved landing earned. A Result with no deferred record is an error, and a
// failed bead write is a *RecordError that leaves the feed unrun and the bead
// untouched (gt-w36i8z).
func TestBookkeepingFinish(t *testing.T) {
	t.Parallel()

	// deferred runs the fixture's landing with the bookkeeping deferred and a
	// review that approves with one minor, so the feed a finish runs is visible.
	deferred := func(t *testing.T) (*landFixture, *Lander, *followupFake, Result) {
		t.Helper()
		f := newLandFixture(t)
		l := f.lander()
		l.DeferBookkeeping = true
		followups := &followupFake{}
		l.Followups = followups
		f.review.fn = func(string) (Verdict, error) {
			return Verdict{Verdict: VerdictApprove, Score: 0.9,
				Findings: []Finding{minorFinding("name the error")}}, nil
		}
		res, err := l.Land(context.Background(), f.work)
		if err != nil {
			t.Fatalf("Land: %v", err)
		}
		if res.Deferred == nil {
			t.Fatal("Result.Deferred is nil after a deferred landing")
		}
		return f, l, followups, res
	}
	// recorded: the bead carries the landing record for rec and is closed,
	// with the ready label off.
	recorded := func(t *testing.T, f *landFixture, rec LandingRecord) {
		t.Helper()
		b := f.bead()
		if b.Status != "closed" {
			t.Errorf("bead status = %q; want closed", b.Status)
		}
		if beads.HasLabel(b, LabelReadyToLand) {
			t.Errorf("bead still carries %s", LabelReadyToLand)
		}
		if !strings.Contains(b.Notes, LandingNoteMarker+"\nlanded_commit: "+rec.LandedCommit) {
			t.Errorf("bead notes = %q; want the landing record for %s", b.Notes, rec.LandedCommit)
		}
	}

	t.Run("writes the bead record and close, then runs the feed", func(t *testing.T) {
		t.Parallel()
		f, l, followups, res := deferred(t)

		if err := l.FinishLanding(f.work, res); err != nil {
			t.Fatalf("FinishLanding: %v", err)
		}
		recorded(t, f, *res.Deferred)
		if n := len(followups.created); n != 1 {
			t.Errorf("filed %d follow-up bead(s) for an approved landing with one minor; want 1", n)
		}
	})

	t.Run("a result with no deferred record is an error", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		l := f.lander()

		err := l.FinishLanding(f.work, Result{Base: f.base, LandedCommit: f.work.Head})
		if err == nil {
			t.Fatal("FinishLanding = nil; want an error for a result that was not deferred")
		}
		b := f.bead()
		if b.Status == "closed" || !beads.HasLabel(b, LabelReadyToLand) || strings.Contains(b.Notes, LandingNoteMarker) {
			t.Errorf("the bead changed on a refused finish: status=%s labels=%v notes=%q", b.Status, b.Labels, b.Notes)
		}
	})

	t.Run("a failed bead write is a RecordError and files no follow-up", func(t *testing.T) {
		t.Parallel()
		f, l, followups, res := deferred(t)
		// Only the record's write fails: the reads stay answerable, so a feed
		// that ran before the write would still file, which is the ordering
		// this case pins.
		l.Beads = updateFails{f.bd}

		err := l.FinishLanding(f.work, res)
		var recErr *RecordError
		if !errors.As(err, &recErr) {
			t.Fatalf("FinishLanding = %v, want a *RecordError from the failed bead write", err)
		}
		if recErr.Result.LandedCommit != res.LandedCommit {
			t.Errorf("RecordError.Result carries %s; want the landing %s", recErr.Result.LandedCommit, res.LandedCommit)
		}
		if n := len(followups.created); n != 0 {
			t.Errorf("filed %d follow-up bead(s) after the bead write failed; want none", n)
		}
		b := f.bead()
		if b.Status == "closed" || !beads.HasLabel(b, LabelReadyToLand) || strings.Contains(b.Notes, LandingNoteMarker) {
			t.Errorf("the bead was written after the bead write failed: status=%s labels=%v notes=%q", b.Status, b.Labels, b.Notes)
		}
	})
}
