package land

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
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
