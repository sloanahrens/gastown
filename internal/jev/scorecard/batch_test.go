package scorecard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
)

// thinLister lists its beads without the fields it names, as a bd list that
// prints fewer fields than bd show would.
type thinLister struct {
	*batchBeads
	dropNotes bool
	listErr   error
	lists     int
}

func (l *thinLister) ListByIDs(ids []string) (map[string]*beads.Issue, error) {
	l.lists++
	if l.listErr != nil {
		return nil, l.listErr
	}
	out := map[string]*beads.Issue{}
	for _, id := range ids {
		if issue, ok := l.issues[id]; ok {
			thin := *issue
			if l.dropNotes {
				thin.Notes = ""
			}
			out[id] = &thin
		}
	}
	return out, nil
}

func fullBeads() map[string]*beads.Issue {
	return map[string]*beads.Issue{
		"gt-a": {ID: "gt-a", Status: "closed", CloseReason: "Landed on main", Notes: "LANDING RECORD", Description: "d"},
		"gt-b": {ID: "gt-b", Status: "open"},
	}
}

// A list that leaves out a field an adapter labels from is not trusted: the
// check against a show finds it, says so, and the beads come from the show
// batch, whole.
func TestShowAllDropsAListThatLacksALabelField(t *testing.T) {
	t.Parallel()
	store := &thinLister{batchBeads: &batchBeads{issues: fullBeads()}, dropNotes: true}

	got, err := showAll(store, []string{"gt-b", "gt-a"})
	if err == nil || !strings.Contains(err.Error(), "notes") || !strings.Contains(err.Error(), "gt-a") {
		t.Errorf("showAll error = %v, want the disagreement on the notes of gt-a", err)
	}
	if got["gt-a"] == nil || got["gt-a"].Notes != "LANDING RECORD" || got["gt-b"] == nil {
		t.Errorf("showAll = %v, want both beads with the notes the show has", got)
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 2 {
		t.Errorf("show batches = %v, want one batch of both ids", store.batches)
	}
}

// A list that agrees with a show is used as it is, and costs the one check.
func TestShowAllKeepsAListThatAgreesWithShow(t *testing.T) {
	t.Parallel()
	store := &thinLister{batchBeads: &batchBeads{issues: fullBeads()}}

	got, err := showAll(store, []string{"gt-a", "gt-b"})
	if err != nil || len(got) != 2 {
		t.Errorf("showAll = %v, %v; want both beads and no error", got, err)
	}
	if len(store.batches) != 0 || len(store.shows) != 1 || store.lists != 1 {
		t.Errorf("calls: %d lists, %d batches, shows %v; want 1 list, no batch and the one check show", store.lists, len(store.batches), store.shows)
	}
}

// A list that fails is reported, and the show batch still reads the beads.
func TestShowAllReportsAFailedListAndFallsBackToTheBatch(t *testing.T) {
	t.Parallel()
	store := &thinLister{batchBeads: &batchBeads{issues: fullBeads()}, listErr: errors.New("list down")}

	got, err := showAll(store, []string{"gt-a", "gt-b"})
	if err == nil || !strings.Contains(err.Error(), "list down") {
		t.Errorf("showAll error = %v, want the list failure", err)
	}
	if len(got) != 2 {
		t.Errorf("showAll = %v, want both beads from the show batch", got)
	}
}

// Nothing listed means nothing to check against.
func TestShowAllChecksNothingWhenNothingIsListed(t *testing.T) {
	t.Parallel()
	store := &thinLister{batchBeads: &batchBeads{issues: fullBeads()}}
	if _, err := showAll(store, []string{"gt-missing"}); err != nil {
		t.Errorf("showAll error = %v", err)
	}
	if len(store.shows) != 0 {
		t.Errorf("showAll read %v with nothing listed", store.shows)
	}
}

// The log's blocks are cut at lines that start with \x01, and a patch cannot
// have one: its lines start with a space, + or -. A \x01 inside a line, at the
// start of a changed line, or in a path stays in its block.
func TestSplitLogBlocksCutsOnlyAtHeaderLines(t *testing.T) {
	t.Parallel()
	log := "\x01aaa bbb\n\n+one \x01 two\n+\x01start\n-\x01gone\n \x01ctx\n" +
		"\x01ccc ddd\n\n+plain\n"
	blocks := splitLogBlocks(log)
	if len(blocks) != 2 {
		t.Fatalf("splitLogBlocks gave %d blocks, want 2: %q", len(blocks), blocks)
	}
	if !strings.HasPrefix(blocks[0], "aaa bbb\n") || !strings.Contains(blocks[0], "+\x01start") || !strings.Contains(blocks[0], " \x01ctx") {
		t.Errorf("first block = %q, want its header and every patch line", blocks[0])
	}
	if !strings.HasPrefix(blocks[1], "ccc ddd\n") || !strings.Contains(blocks[1], "+plain") {
		t.Errorf("second block = %q", blocks[1])
	}
}

// gitCounter counts the git calls rigDiffs makes and fails each of them the way
// git fails when it cannot read the repo: an exit that is not status 1.
type gitCounter struct {
	mu    sync.Mutex
	calls [][]string
}

func (g *gitCounter) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, append([]string(nil), args...))
	return nil, &exec.ExitError{ProcessState: &os.ProcessState{}}
}

// A prefetch whose git call fails records nothing and says so, and the
// commits are then sized by their own calls: the run costs time, not a label.
func TestPrefetchLinesGitFailureLeavesTheCommitsToTheirOwnCalls(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, dedupe.RigName, ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := &gitCounter{}
	sizes := &diffSizes{}
	d := rigDiffs{town: town, sizes: sizes, run: git.run}

	d.PrefetchLines(context.Background(), []string{"abc1234", "def5678", "abc1234"})
	if len(git.calls) != 1 || !strings.Contains(strings.Join(git.calls[0], " "), "cat-file") {
		t.Errorf("a failing prefetch made git calls %v, want the one cat-file", git.calls)
	}
	if len(sizes.lines) != 0 || len(sizes.absent) != 0 {
		t.Errorf("a failed prefetch recorded %v and %v", sizes.lines, sizes.absent)
	}

	if _, err := d.ChangedLines(context.Background(), "abc1234"); err == nil || errors.Is(err, ErrNoCommit) {
		t.Errorf("ChangedLines after a failed prefetch = %v, want the git failure and not ErrNoCommit", err)
	}
	if len(git.calls) != 2 || !strings.Contains(strings.Join(git.calls[1], " "), "rev-parse") {
		t.Errorf("git calls after the fallback = %v, want it to ask git itself", git.calls)
	}
}

// Priority and Labels are read by the stale and dispatch batteries, so a list
// and a show that differ only on either disagree, and the check says which.
func TestLabelFieldsDetectsPriorityAndLabelChange(t *testing.T) {
	t.Parallel()
	base := func() *beads.Issue {
		return &beads.Issue{ID: "gt-a", Status: "open", Priority: 2, Labels: []string{"gt:bug"}}
	}
	cases := []struct {
		name   string
		change func(*beads.Issue)
		field  string
	}{
		{"priority", func(i *beads.Issue) { i.Priority = 0 }, "priority"},
		{"labels", func(i *beads.Issue) { i.Labels = []string{"gt:bug", "needs-split"} }, "labels"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			shown := base()
			c.change(shown)
			if got := firstLabelFieldDiff(base(), shown); got != c.field {
				t.Errorf("firstLabelFieldDiff = %q, want %q", got, c.field)
			}
		})
	}
}
