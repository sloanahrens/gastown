package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// commentCounting counts the comment reads made against a store.
type commentCounting struct {
	beads.Client
	reads map[string]int
}

func (c *commentCounting) Comments(id string) ([]beads.Comment, error) {
	c.reads[id]++
	return c.Client.Comments(id)
}

// annotatedTick is one tick's annotation of held beads, over a store: the board
// row of each carries the store's comment count, the memory is loaded from path
// as a new tick would, and each held bead is annotated with its note.
func annotatedTick(t *testing.T, db *beadsfake.Fake, path, owner string, held []string) map[string]int {
	t.Helper()
	store := &commentCounting{Client: db, reads: map[string]int{}}
	var board specBoardRead
	for _, id := range held {
		comments, err := db.Comments(id)
		if err != nil {
			t.Fatal(err)
		}
		board.Candidates = append(board.Candidates, specCandidate{Spec: specdispatch.Spec{ID: id, CommentCount: len(comments)}})
	}
	a := loadSpecAnnotated(path, owner)
	a.observe(board)
	for _, id := range held {
		note := "SHAPE " + id + ": missing ## Gate"
		err := a.once(id, note, func() (bool, error) { return annotateSpecWrote(store, id, note, note) })
		if err != nil {
			t.Fatalf("annotate %s: %v", id, err)
		}
	}
	a.save()
	return store.reads
}

func annotatedStore(t *testing.T, ids ...string) *beadsfake.Fake {
	t.Helper()
	db := beadsfake.New()
	for _, id := range ids {
		db.Seed(beads.Issue{ID: id, Title: id, Status: "open"})
	}
	return db
}

func commentsOn(t *testing.T, db *beadsfake.Fake, id string) []string {
	t.Helper()
	cs, err := db.Comments(id)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, c := range cs {
		texts = append(texts, c.Text)
	}
	return texts
}

func TestSpecAnnotatedSecondTickReadsNoComments(t *testing.T) {
	t.Parallel()
	held := []string{"gt-a", "gt-b", "gt-c"}
	db := annotatedStore(t, held...)
	path := filepath.Join(t.TempDir(), "daemon", "spec_annotated.json")

	first := annotatedTick(t, db, path, "daemon-1", held)
	for _, id := range held {
		if first[id] != 1 {
			t.Errorf("first tick read %s's comments %d times, want 1", id, first[id])
		}
	}
	second := annotatedTick(t, db, path, "daemon-1", held)
	if len(second) != 0 {
		t.Errorf("second tick read comments of %v, want none", second)
	}
	for _, id := range held {
		if got := commentsOn(t, db, id); len(got) != 1 {
			t.Errorf("%s carries %d notes, want 1: %q", id, len(got), got)
		}
	}
}

func TestSpecAnnotatedChangedCountIsReadAgain(t *testing.T) {
	t.Parallel()
	held := []string{"gt-a", "gt-b"}
	db := annotatedStore(t, held...)
	path := filepath.Join(t.TempDir(), "spec_annotated.json")
	annotatedTick(t, db, path, "daemon-1", held)

	if err := db.AddComment("gt-b", "operator: looking at this"); err != nil {
		t.Fatal(err)
	}
	reads := annotatedTick(t, db, path, "daemon-1", held)
	if reads["gt-a"] != 0 || reads["gt-b"] != 1 {
		t.Errorf("reads = %v, want only gt-b read once", reads)
	}
	if got := commentsOn(t, db, "gt-b"); len(got) != 2 {
		t.Errorf("gt-b carries %q, want the note once beside the operator's comment", got)
	}
	if again := annotatedTick(t, db, path, "daemon-1", held); len(again) != 0 {
		t.Errorf("tick after the re-read read %v, want none", again)
	}
}

func TestSpecAnnotatedLostMemoryReadsBeforeWriting(t *testing.T) {
	t.Parallel()
	held := []string{"gt-a", "gt-b"}
	db := annotatedStore(t, held...)
	path := filepath.Join(t.TempDir(), "spec_annotated.json")
	annotatedTick(t, db, path, "daemon-1", held)

	for name, reset := range map[string]func(){
		"file removed":    func() { _ = os.Remove(path) },
		"file unreadable": func() { _ = os.WriteFile(path, []byte("{not json"), 0o600) },
		"another daemon":  func() {},
	} {
		reset()
		owner := "daemon-1"
		if name == "another daemon" {
			owner = "daemon-2"
		}
		reads := annotatedTick(t, db, path, owner, held)
		for _, id := range held {
			if reads[id] != 1 {
				t.Errorf("%s: read %s's comments %d times, want 1", name, id, reads[id])
			}
			if got := commentsOn(t, db, id); len(got) != 1 {
				t.Errorf("%s: %s carries %d notes, want 1", name, id, len(got))
			}
		}
	}
}

func TestSpecAnnotatedKeepsNothingForAHandRunTick(t *testing.T) {
	t.Parallel()
	held := []string{"gt-a"}
	db := annotatedStore(t, held...)
	path := filepath.Join(t.TempDir(), "spec_annotated.json")
	annotatedTick(t, db, path, "", held)
	if _, err := os.Stat(path); err == nil {
		t.Error("a tick no daemon started wrote the annotated set")
	}
	if reads := annotatedTick(t, db, path, "", held); reads["gt-a"] != 1 {
		t.Errorf("hand-run tick read %v, want gt-a read", reads)
	}
}

func TestSpecAnnotatedDifferentKeyOnAnnotatedBeadIsWritten(t *testing.T) {
	t.Parallel()
	db := annotatedStore(t, "gt-a")
	path := filepath.Join(t.TempDir(), "spec_annotated.json")
	annotatedTick(t, db, path, "daemon-1", []string{"gt-a"})

	store := &commentCounting{Client: db, reads: map[string]int{}}
	a := loadSpecAnnotated(path, "daemon-1")
	a.observe(specBoardRead{Candidates: []specCandidate{{Spec: specdispatch.Spec{ID: "gt-a", CommentCount: 1}}}})
	other := "SHAPE gt-a: a different verdict"
	if err := a.once("gt-a", other, func() (bool, error) { return annotateSpecWrote(store, "gt-a", other, other) }); err != nil {
		t.Fatal(err)
	}
	got := commentsOn(t, db, "gt-a")
	if len(got) != 2 || !strings.HasPrefix(got[1], other) {
		t.Errorf("comments = %q, want the new verdict beside the first", got)
	}
}

func TestSpecAnnotatedSavePrunesToListedBeads(t *testing.T) {
	t.Parallel()
	db := annotatedStore(t, "gt-a", "gt-b")
	path := filepath.Join(t.TempDir(), "spec_annotated.json")
	annotatedTick(t, db, path, "daemon-1", []string{"gt-a", "gt-b"})
	annotatedTick(t, db, path, "daemon-1", []string{"gt-a"})

	a := loadSpecAnnotated(path, "daemon-1")
	if _, kept := a.seen["gt-b"]; kept {
		t.Error("the set still holds gt-b after the board stopped listing it")
	}
	if _, kept := a.seen["gt-a"]; !kept {
		t.Error("the set dropped gt-a, which the board still lists")
	}
}

// TestSpecAnnotatedTickWiring drives the memory through the wiring the tick
// runs rather than a copy of it: a first tick annotates a held bead once, and
// a second tick over the same board makes no comment read (gt-fpxlb).
func TestSpecAnnotatedTickWiring(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := annotatedStore(t, "gt-a")
	note := "SHAPE gt-a: missing ## Gate"

	tick := func() map[string]int {
		store := &commentCounting{Client: db, reads: map[string]int{}}
		annotations := newSpecAnnotatedTick(root, "daemon-1", func(string) beads.Client { return store })
		comments, err := db.Comments("gt-a")
		if err != nil {
			t.Fatal(err)
		}
		annotations.observe(specBoardRead{Candidates: []specCandidate{{Spec: specdispatch.Spec{ID: "gt-a", CommentCount: len(comments)}}}})
		if err := annotations.annotate("gt-a", note, note); err != nil {
			t.Fatalf("annotate: %v", err)
		}
		annotations.save()
		return store.reads
	}

	if reads := tick(); reads["gt-a"] != 1 {
		t.Errorf("first tick read gt-a's comments %d times, want 1", reads["gt-a"])
	}
	if _, err := os.Stat(dispatch.SpecAnnotatedPath(root)); err != nil {
		t.Errorf("the tick kept no memory at %s: %v", dispatch.SpecAnnotatedPath(root), err)
	}
	if reads := tick(); len(reads) != 0 {
		t.Errorf("second tick read %v, want no comment reads", reads)
	}
	if got := commentsOn(t, db, "gt-a"); len(got) != 1 {
		t.Errorf("gt-a carries %d notes, want 1: %q", len(got), got)
	}
}

// TestSpecAnnotatedSecondKeySameTickIsNotReadAgain is the regression test for
// the stale first entry: two note keys on one bead in one tick both leave the
// bead remembered at its board count, so the next tick reads nothing (gt-fpxlb).
func TestSpecAnnotatedSecondKeySameTickIsNotReadAgain(t *testing.T) {
	t.Parallel()
	db := annotatedStore(t, "gt-a")
	path := filepath.Join(t.TempDir(), "daemon", "spec_annotated.json")
	notes := []string{"SHAPE gt-a: missing ## Gate", "HOLD gt-a: red main in flight"}

	store := &commentCounting{Client: db, reads: map[string]int{}}
	a := loadSpecAnnotated(path, "daemon-1")
	a.observe(specBoardRead{Candidates: []specCandidate{{Spec: specdispatch.Spec{ID: "gt-a"}}}})
	for _, note := range notes {
		if err := a.once("gt-a", note, func() (bool, error) { return annotateSpecWrote(store, "gt-a", note, note) }); err != nil {
			t.Fatalf("annotate %q: %v", note, err)
		}
	}
	a.save()

	if got := commentsOn(t, db, "gt-a"); len(got) != len(notes) {
		t.Fatalf("gt-a carries %q, want both notes", got)
	}
	comments, err := db.Comments("gt-a")
	if err != nil {
		t.Fatal(err)
	}
	next := &commentCounting{Client: db, reads: map[string]int{}}
	b := loadSpecAnnotated(path, "daemon-1")
	b.observe(specBoardRead{Candidates: []specCandidate{{Spec: specdispatch.Spec{ID: "gt-a", CommentCount: len(comments)}}}})
	for _, note := range notes {
		if err := b.once("gt-a", note, func() (bool, error) { return annotateSpecWrote(next, "gt-a", note, note) }); err != nil {
			t.Fatalf("annotate %q: %v", note, err)
		}
	}
	if len(next.reads) != 0 {
		t.Errorf("the tick after two notes read %v, want no comment reads", next.reads)
	}
	if got := commentsOn(t, db, "gt-a"); len(got) != len(notes) {
		t.Errorf("gt-a carries %q, want each note once", got)
	}
}

// TestSpecAnnotatedTicksRunConcurrently is the regression test for the
// unsynchronized memory maps: the tick's callbacks are function values the
// dispatcher holds, so a second caller must cost a slow read rather than a map
// race. Run under -race, as CI does (gt-fpxlb).
func TestSpecAnnotatedTicksRunConcurrently(t *testing.T) {
	t.Parallel()
	a := loadSpecAnnotated(filepath.Join(t.TempDir(), "spec_annotated.json"), "daemon-1")
	board := specBoardRead{Candidates: []specCandidate{{Spec: specdispatch.Spec{ID: "gt-a"}}}}

	var round sync.WaitGroup
	for range 8 {
		round.Add(1)
		go func() {
			defer round.Done()
			for range 25 {
				a.observe(board)
				_ = a.once("gt-a", "SHAPE gt-a", func() (bool, error) { return false, nil })
				a.save()
			}
		}()
	}
	round.Wait()
}

func TestSpecAnnotatedFailedReadIsNotRemembered(t *testing.T) {
	t.Parallel()
	a := loadSpecAnnotated("", "")
	a.observe(specBoardRead{Candidates: []specCandidate{{Spec: specdispatch.Spec{ID: "gt-a"}}}})
	calls := 0
	failing := func() (bool, error) { calls++; return false, os.ErrDeadlineExceeded }
	for i := 0; i < 2; i++ {
		if err := a.once("gt-a", "SHAPE gt-a", failing); err == nil {
			t.Fatal("once swallowed the read error")
		}
	}
	if calls != 2 {
		t.Errorf("annotate ran %d times, want 2: a failed read must be retried", calls)
	}
}

// TestTickEffQuietRigMemory: a rig answers quiet only when its hashes equal the
// stored read, which listed no candidate and ran under five minutes before now.
func TestTickEffQuietRigMemory(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	stored := beadsql.RigHash{Head: "h1", Working: "w1"}
	cases := []struct {
		name       string
		query      string
		heads      map[string]beadsql.RigHash
		candidates int
		now        time.Time
		want       bool
	}{
		{"unchanged and quiet", "gt", map[string]beadsql.RigHash{"gt": stored}, 0, base.Add(time.Minute), true},
		{"just under five minutes old", "gt", map[string]beadsql.RigHash{"gt": stored}, 0, base.Add(5*time.Minute - time.Second), true},
		{"five minutes old", "gt", map[string]beadsql.RigHash{"gt": stored}, 0, base.Add(5 * time.Minute), false},
		{"read stamped after now", "gt", map[string]beadsql.RigHash{"gt": stored}, 0, base.Add(-time.Second), false},
		{"head moved", "gt", map[string]beadsql.RigHash{"gt": {Head: "h2", Working: "w1"}}, 0, base.Add(time.Minute), false},
		{"working moved", "gt", map[string]beadsql.RigHash{"gt": {Head: "h1", Working: "w2"}}, 0, base.Add(time.Minute), false},
		{"candidate listed", "gt", map[string]beadsql.RigHash{"gt": stored}, 1, base.Add(time.Minute), false},
		{"empty hash", "gt", map[string]beadsql.RigHash{"gt": {}}, 0, base.Add(time.Minute), false},
		{"rig absent from hashes", "gt", map[string]beadsql.RigHash{"be": stored}, 0, base.Add(time.Minute), false},
		{"rig absent from memory", "be", map[string]beadsql.RigHash{"be": stored}, 0, base.Add(time.Minute), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := loadSpecAnnotated(filepath.Join(t.TempDir(), "spec_annotated.json"), "daemon-1")
			a.noteRigRead("gt", stored, base, tc.candidates)
			if got := a.rigQuiet(tc.query, tc.heads, tc.now); got != tc.want {
				t.Errorf("rigQuiet(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// quietRigSaved saves gt's unchanged, quiet read under owner and returns the file.
func quietRigSaved(t *testing.T, owner string, base time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec_annotated.json")
	a := loadSpecAnnotated(path, owner)
	a.noteRigRead("gt", beadsql.RigHash{Head: "h1", Working: "w1"}, base, 0)
	a.save()
	return path
}

// TestTickEffQuietRigMemoryAcrossDaemons: the next tick under the same daemon
// loads the saved read, and a restarted daemon, with a new owner, loads cold
// (gt-wduoe3).
func TestTickEffQuietRigMemoryAcrossDaemons(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	path := quietRigSaved(t, "daemon-1", base)
	heads := map[string]beadsql.RigHash{"gt": {Head: "h1", Working: "w1"}}
	t.Run("same owner keeps the read", func(t *testing.T) {
		if !loadSpecAnnotated(path, "daemon-1").rigQuiet("gt", heads, base.Add(time.Minute)) {
			t.Error("the next tick under the same daemon reads gt as not quiet, want the saved read kept")
		}
	})
	t.Run("new owner starts cold", func(t *testing.T) {
		if loadSpecAnnotated(path, "daemon-2").rigQuiet("gt", heads, base.Add(time.Minute)) {
			t.Error("a restarted daemon reads gt as quiet, want a cold start")
		}
	})
}
