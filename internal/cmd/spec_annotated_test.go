package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
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
