package submission

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/land"
)

// The pass's git runs only in its own bare clone, under .runtime/jev/repos,
// never in the rig's mayor/rig checkout or any town worktree.
func TestJevSubmissionGitRunsOnlyInItsOwnClone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clone := ClonePath(town)
	if want := filepath.Join(town, constants.DirRuntime, "jev", "repos", "gastown.git"); clone != want {
		t.Fatalf("ClonePath = %q, want %q", clone, want)
	}
	if strings.Contains(clone, filepath.Join("mayor", "rig")) {
		t.Fatalf("ClonePath names the mayor clone: %q", clone)
	}

	var dirs, calls []string
	run := func(_ context.Context, dir string, args ...string) (string, error) {
		dirs = append(dirs, dir)
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "config":
			return "", errors.New("no such key")
		case "log":
			return "fix: a\ndocs: b\n", nil
		case "diff":
			return "1\t2\ta.go\n", nil
		}
		return "", nil
	}
	g := &bareGit{dir: clone, origin: "git@example.com:gastown.git", timeout: time.Second, run: run}

	ctx := context.Background()
	if err := g.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	subjects, err := g.CommitSubjects(ctx, "main", "head", maxSubjects)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 2 || subjects[0] != "fix: a" {
		t.Errorf("subjects = %v", subjects)
	}
	stat, err := g.Numstat(ctx, "main", "head")
	if err != nil {
		t.Fatal(err)
	}
	if stat != "1\t2\ta.go\n" {
		t.Errorf("numstat = %q", stat)
	}

	if len(dirs) == 0 {
		t.Fatal("the runner was never called")
	}
	repos := filepath.Join(town, constants.DirRuntime, "jev", "repos")
	for _, dir := range dirs {
		if dir != clone {
			t.Errorf("git ran in %q, want the clone %q (calls: %v)", dir, clone, calls)
		}
		if rel, err := filepath.Rel(repos, dir); err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("git ran outside %q: %q", repos, dir)
		}
	}
	if !strings.Contains(strings.Join(calls, "\n"), "remote add origin git@example.com:gastown.git") {
		t.Errorf("the clone was not pointed at the rig origin: %v", calls)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "fetch --prune --quiet origin +refs/heads/*:refs/heads/*") {
		t.Errorf("the clone was not fetched: %v", calls)
	}
}

// A ref that starts with "-" is refused before git runs: a READY TO LAND
// block must not be able to pass git an option.
func TestJevSubmissionRefusesOptionRefs(t *testing.T) {
	t.Parallel()
	var calls int
	run := func(context.Context, string, ...string) (string, error) {
		calls++
		return "", nil
	}
	g := &bareGit{dir: t.TempDir(), origin: "origin", timeout: time.Second, run: run}

	if _, err := g.CommitSubjects(context.Background(), "--all", "head", 1); err == nil {
		t.Error("CommitSubjects accepted a ref that starts with -")
	}
	if _, err := g.Numstat(context.Background(), "main", "-R"); err == nil {
		t.Error("Numstat accepted a ref that starts with -")
	}
	if calls != 0 {
		t.Errorf("a refused ref still ran git %d times", calls)
	}
}

// recordingLister records each query and answers waiting/closed by the
// filter it carries.
type recordingLister struct {
	calls   []beads.ListOptions
	waiting []*beads.Issue
	closed  []*beads.Issue
}

func (r *recordingLister) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	r.calls = append(r.calls, opts)
	if opts.Label != "" {
		return r.waiting, nil
	}
	return r.closed, nil
}

// The production lister asks for the ready-to-land label and for the beads
// closed since the cursor, keeps only actionable waiting beads, and returns a
// bead once when both reads see it.
func TestJevSubmissionListerReadsWaitingAndClosedBeads(t *testing.T) {
	t.Parallel()
	since := t0.Add(-time.Hour)
	rec := &recordingLister{
		waiting: []*beads.Issue{
			{ID: "gt-open", Status: "open"},
			{ID: "gt-hooked", Status: beads.StatusHooked},
			{ID: "gt-deferred", Status: string(beads.StatusDeferred)},
			{ID: "gt-both", Status: "open"},
			nil,
		},
		closed: []*beads.Issue{
			{ID: "gt-landed", Status: "closed"},
			{ID: "gt-both", Status: "closed"},
			nil,
		},
	}

	got, err := rigBeads{b: rec}.Since(since)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("made %d list calls, want 2", len(rec.calls))
	}
	if rec.calls[0].Label != land.LabelReadyToLand {
		t.Errorf("first call Label = %q, want %q", rec.calls[0].Label, land.LabelReadyToLand)
	}
	if rec.calls[1].Status != "closed" || !rec.calls[1].ClosedAfter.Equal(since) {
		t.Errorf("second call = %+v, want the beads closed since %v", rec.calls[1], since)
	}
	var ids []string
	for _, issue := range got {
		ids = append(ids, issue.ID)
	}
	if strings.Join(ids, " ") != "gt-open gt-hooked gt-both gt-landed" {
		t.Errorf("Since() = %v, want the actionable waiting beads then the closed ones", ids)
	}
}
