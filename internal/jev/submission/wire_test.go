package submission

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/battery"
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

// recordingLister records each query, answers the label query with waiting
// and resolves beads by ID through Show.
type recordingLister struct {
	calls   []beads.ListOptions
	waiting []*beads.Issue
	byID    map[string]*beads.Issue
	// failing names beads whose read fails with the error given.
	failing map[string]error
}

func (r *recordingLister) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	r.calls = append(r.calls, opts)
	return r.waiting, nil
}

func (r *recordingLister) Show(id string) (*beads.Issue, error) {
	if err := r.failing[id]; err != nil {
		return nil, err
	}
	if issue, ok := r.byID[id]; ok {
		return issue, nil
	}
	return nil, beads.ErrNotFound
}

// The lister reads the first-sight records since the cursor, then lists the
// waiting beads that no record names. A record older than the cursor, or one
// whose bead is gone, yields nothing; a waiting bead with a record at its head
// yields nothing, but at a new head it is listed again.
func TestJevSubmissionListerReadsRecordsThenUnrecordedWaitingBeads(t *testing.T) {
	t.Parallel()
	since := t0.Add(-time.Hour)
	records := filepath.Join(t.TempDir(), land.SubmissionsSeenFile)
	for _, rec := range []land.SubmissionRecord{
		{SeenAt: since.Add(-time.Minute), Bead: "gt-old", Head: "h-old", Target: "main"},
		{SeenAt: since.Add(time.Minute), Bead: "gt-recorded", Head: "h-rec", Target: "main", Subjects: []string{"fix: it (gt-recorded)"}, Numstat: "1\t1\tx.go"},
		{SeenAt: since.Add(2 * time.Minute), Bead: "gt-gone", Head: "h-gone", Target: "main"},
		{SeenAt: since.Add(-2 * time.Minute), Bead: "gt-resub", Head: "h-first", Target: "main"},
	} {
		if err := battery.Append(records, rec); err != nil {
			t.Fatal(err)
		}
	}
	ready := func(id, head string) *beads.Issue {
		return &beads.Issue{ID: id, Status: "open", Labels: []string{land.LabelReadyToLand}, Notes: land.FormatReadyNote(land.Work{
			Branch: "polecat/x/" + id, Head: head, Target: "main", Submitted: since.Add(4 * time.Minute),
		})}
	}
	deferred := ready("gt-deferred", "h-d")
	deferred.Status = string(beads.StatusDeferred)
	rec := &recordingLister{
		waiting: []*beads.Issue{
			{ID: "gt-open", Status: "open"},
			ready("gt-old", "h-old"),
			ready("gt-recorded", "h-rec"),
			ready("gt-resub", "h-second"),
			deferred,
			nil,
		},
		byID: map[string]*beads.Issue{
			"gt-recorded": {ID: "gt-recorded", Status: "closed"},
			"gt-old":      {ID: "gt-old", Status: "closed"},
		},
	}

	got, err := rigBeads{b: rec, records: records}.Since(since)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || rec.calls[0].Label != land.LabelReadyToLand {
		t.Errorf("list calls = %+v, want one read of the ready-to-land label", rec.calls)
	}
	var ids []string
	for _, e := range got {
		ids = append(ids, e.Issue.ID)
	}
	if strings.Join(ids, " ") != "gt-recorded gt-resub" {
		t.Fatalf("Since() = %v, want the recorded submission then the resubmitted one", ids)
	}
	if !got[0].Seen.Equal(since.Add(time.Minute)) || got[0].Work.Head != "h-rec" || got[0].Numstat != "1\t1\tx.go" || len(got[0].Subjects) != 1 {
		t.Errorf("recorded entry = %+v, want the record's seen time, head, subjects and numstat", got[0])
	}
	if got[1].Work.Head != "h-second" || !got[1].Seen.Equal(since.Add(4*time.Minute)) || got[1].Subjects != nil {
		t.Errorf("waiting entry = %+v, want the new head at its submission time with no record subjects", got[1])
	}
}

// A record whose bead cannot be read is reported and left out, and the records
// behind it are still listed, so one bad read does not stop the pass.
func TestJevSubmissionListerSkipsARecordWhoseBeadCannotBeRead(t *testing.T) {
	t.Parallel()
	since := t0.Add(-time.Hour)
	records := filepath.Join(t.TempDir(), land.SubmissionsSeenFile)
	for _, rec := range []land.SubmissionRecord{
		{SeenAt: since.Add(time.Minute), Bead: "gt-broken", Head: "h-broken", Target: "main"},
		{SeenAt: since.Add(2 * time.Minute), Bead: "gt-fine", Head: "h-fine", Target: "main"},
	} {
		if err := battery.Append(records, rec); err != nil {
			t.Fatal(err)
		}
	}
	rec := &recordingLister{
		byID:    map[string]*beads.Issue{"gt-fine": {ID: "gt-fine", Status: "closed"}},
		failing: map[string]error{"gt-broken": errors.New("dolt: connection reset")},
	}
	var warnings bytes.Buffer

	got, err := rigBeads{b: rec, records: records, warnings: &warnings}.Since(since)
	if err != nil {
		t.Fatalf("Since() error = %v, want the pass to go on", err)
	}
	if len(got) != 1 || got[0].Issue.ID != "gt-fine" {
		t.Fatalf("Since() = %+v, want only gt-fine", got)
	}
	if !strings.Contains(warnings.String(), "gt-broken") || !strings.Contains(warnings.String(), "connection reset") {
		t.Errorf("warnings = %q, want the skipped bead and its error", warnings.String())
	}
}

// A submission that already landed under a --no-ff merge has its head
// reachable from target, so target..head has no commits. Git still reports a
// non-empty diff for that range (the changes target gained since), so the pass
// must skip it before Jev is asked, and log nothing for it.
func TestAskLandedSubmissionNotScoredOnEmptyDiff(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	landed := r.ready("gt-landed", time.Minute)
	r.git.subjects["head-gt-landed"] = nil
	r.git.numstat["head-gt-landed"] = "40\t3\tinternal/other.go\n"
	r.submissions.issues = []*beads.Issue{landed}

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 0 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v, want 0 asked and 1 skipped", sum)
	}
	if n := len(r.asker.asked); n != 0 {
		t.Errorf("Jev was asked %d times about a landed submission with no commits", n)
	}
	if _, err := os.Stat(r.pass.LogPath); !os.IsNotExist(err) {
		t.Errorf("a landed submission wrote a log: %v", err)
	}
	cur, ok, _ := battery.ReadCursor(r.pass.CursorPath)
	if !ok || cur.ID != "gt-landed" {
		t.Errorf("cursor = %+v ok=%v, want it past gt-landed", cur, ok)
	}
}
