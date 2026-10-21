package submission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
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

// gitEnv drops the variables that pick a repository and keeps the rest.
func TestJevSubmissionGitEnvDropsRepositoryVariables(t *testing.T) {
	t.Parallel()
	got := gitEnv([]string{
		"PATH=/usr/bin", "GIT_DIR=/elsewhere/.git", "GIT_WORK_TREE=/elsewhere",
		"GIT_INDEX_FILE=/elsewhere/index", "GIT_SSH_COMMAND=ssh -i key", "HOME=/home/x",
	})
	want := []string{"PATH=/usr/bin", "GIT_SSH_COMMAND=ssh -i key", "HOME=/home/x"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("gitEnv = %v, want %v", got, want)
	}
}

// runGit reads the repository -C names even when the caller carries a GIT_DIR
// that points elsewhere, as a process started from a git hook does.
func TestJevSubmissionRunGitIgnoresCallerGitDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := runGit(context.Background(), dir, "init", "--bare", "--quiet"); err != nil {
		t.Fatal(err)
	}
	foreign := append(os.Environ(), "GIT_DIR="+filepath.Join(t.TempDir(), "not-a-repo"))

	out, err := runGitEnv(context.Background(), foreign, dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		t.Fatalf("runGit with a foreign GIT_DIR: %v", err)
	}
	if strings.TrimSpace(out) != "true" {
		t.Errorf("rev-parse = %q, want it to read the clone at dir", out)
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

// A log longer than the read window is trimmed of the records no pass can still
// ask about: each seen before the cursor whose bead is not waiting at its head.
// The waiting bead's old record survives the trim, so the bead is still not
// asked twice, and the record past the cursor is asked once.
func TestJevSubmissionListerTrimsRecordsNoPassCanAskAbout(t *testing.T) {
	t.Parallel()
	since := t0.Add(-time.Hour)
	records := filepath.Join(t.TempDir(), land.SubmissionsSeenFile)
	for i := range recordWindow {
		rec := land.SubmissionRecord{SeenAt: since.Add(-time.Duration(i+1) * time.Second), Bead: fmt.Sprintf("gt-done-%d", i), Head: "h-done", Target: "main"}
		if err := battery.Append(records, rec); err != nil {
			t.Fatal(err)
		}
	}
	for _, rec := range []land.SubmissionRecord{
		{SeenAt: since.Add(-time.Hour), Bead: "gt-held", Head: "h-held", Target: "main"},
		{SeenAt: since.Add(time.Minute), Bead: "gt-new", Head: "h-new", Target: "main"},
	} {
		if err := battery.Append(records, rec); err != nil {
			t.Fatal(err)
		}
	}
	held := &beads.Issue{ID: "gt-held", Status: "open", Labels: []string{land.LabelReadyToLand}, Notes: land.FormatReadyNote(land.Work{
		Branch: "polecat/x/gt-held", Head: "h-held", Target: "main", Submitted: since.Add(-time.Hour),
	})}
	rec := &recordingLister{
		waiting: []*beads.Issue{held},
		byID:    map[string]*beads.Issue{"gt-new": {ID: "gt-new", Status: "closed"}},
	}

	got, err := rigBeads{b: rec, records: records}.Since(since)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range got {
		ids = append(ids, e.Issue.ID)
	}
	if strings.Join(ids, " ") != "gt-new" {
		t.Fatalf("Since() = %v, want only the record past the cursor", ids)
	}
	kept, err := land.ReadSubmissionRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	var beadsKept []string
	for _, k := range kept {
		beadsKept = append(beadsKept, k.Bead)
	}
	if strings.Join(beadsKept, " ") != "gt-held gt-new" {
		t.Fatalf("log after the trim holds %v, want the waiting bead's record and the one past the cursor", beadsKept)
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
	var warnings bytes.Buffer
	r.pass.Warnings = &warnings

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 0 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v, want 0 asked and 1 skipped", sum)
	}
	if got := warnings.String(); !strings.Contains(got, "gt-landed") || !strings.Contains(got, "already landed") || strings.Contains(got, "unreadable") {
		t.Errorf("warnings = %q, want the skipped bead named as landed, not unreadable", got)
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

// jevReply is a Jev reply to the battery's questions. Its outcome probabilities
// are the JSON object given, so one reply can carry its keys in any order.
func jevReply(outcome string) string {
	return fmt.Sprintf(`{
  "model": "jev-1.13.0",
  "answers": {
    "tests_cover_change": {"type": "noul", "noul": 0.7},
    "beyond_goal": {"type": "noul", "noul": 0.1},
    "commits_match_change": {"type": "noul", "noul": 0.9},
    "touches_town_machinery": {"type": "noul", "noul": 0.2},
    "will_pass_review": {"type": "noul", "noul": 0.8},
    "claims_verified": {"type": "noul", "noul": 0.9},
    "verification_applies": {"type": "noul", "noul": 0.6},
    "boxes_credible": {"type": "noul", "noul": 0.7},
    "risk": {"type": "score", "score": 1, "confidence": 0.6,
      "legend": {"0": "docs", "1": "one package", "2": "across packages", "3": "town machinery"},
      "probabilities": {"0": 0.1, "1": 0.6, "2": 0.2, "3": 0.1}},
    "outcome": {"type": "choice", "choice": "complete", "confidence": 0.7,
      "probabilities": %s}
  },
  "usage": {"input_tokens": 100, "output_tokens": 20}
}`, outcome)
}

// replyTo is a transport that records each request body and answers with
// reply, so the test drives the real client without listening on a port.
type replyTo struct {
	reply    string
	requests [][]byte
}

func (rt *replyTo) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	rt.requests = append(rt.requests, body)
	rec := httptest.NewRecorder()
	_, _ = io.WriteString(rec, rt.reply)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

// One Jev call carries every question the battery asks, the state holds the
// MR notes, and the outcome's answer reaches the line whatever order Jev sends
// its probabilities in: the same values in two orders give the same answer.
func TestJevSubmissionAskCarriesTheNewQuestions(t *testing.T) {
	t.Parallel()
	orders := []struct {
		name, probabilities string
	}{
		{"sorted keys", `{"blocked": 0.05, "complete": 0.7, "partial": 0.2, "unclear": 0.05}`},
		{"reversed keys", `{"unclear": 0.05, "partial": 0.2, "complete": 0.7, "blocked": 0.05}`},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			t.Parallel()
			rt := &replyTo{reply: jevReply(order.probabilities)}
			client, err := jev.NewClient(
				jev.WithBaseURL("http://jev.invalid/v1/systemone"),
				jev.WithKey("test-key"),
				jev.WithHTTPClient(&http.Client{Transport: rt}),
			)
			if err != nil {
				t.Fatal(err)
			}

			r := newRig(t)
			r.pass.Asker = client
			r.submissions.issues = []*beads.Issue{r.ready("gt-a", time.Minute)}
			sum, err := r.pass.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if sum.Asked != 1 {
				t.Fatalf("summary = %+v, want the submission asked", sum)
			}

			if len(rt.requests) != 1 {
				t.Fatalf("Jev was called %d times, want one call for the whole battery", len(rt.requests))
			}
			var sent struct {
				State     string                     `json:"state"`
				Questions map[string]json.RawMessage `json:"questions"`
			}
			if err := json.Unmarshal(rt.requests[0], &sent); err != nil {
				t.Fatal(err)
			}
			if len(sent.Questions) != len(Questions()) {
				t.Errorf("request asks %d questions, want %d", len(sent.Questions), len(Questions()))
			}
			for _, id := range []string{QClaimsVerified, QVerificationApplies, QBoxesCredible, QOutcome} {
				if _, ok := sent.Questions[id]; !ok {
					t.Errorf("request does not ask %s", id)
				}
			}
			if _, ok := stateFields(t, sent.State)["mr_notes"]; !ok {
				t.Errorf("state holds no mr_notes field:\n%s", sent.State)
			}

			lines := readLines(t, r.pass.LogPath)
			if len(lines) != 1 {
				t.Fatalf("log holds %d lines, want 1", len(lines))
			}
			got := lines[0].Answers
			if n := got[QClaimsVerified].Noul; n == nil || *n != 0.9 {
				t.Errorf("claims_verified = %+v, want noul 0.9", got[QClaimsVerified])
			}
			if n := got[QBoxesCredible].Noul; n == nil || *n != 0.7 {
				t.Errorf("boxes_credible = %+v, want noul 0.7", got[QBoxesCredible])
			}
			outcome := got[QOutcome]
			if outcome.Choice != OutcomeComplete || outcome.Positive != OutcomeComplete {
				t.Errorf("outcome = %+v, want complete with complete positive", outcome)
			}
			if outcome.Probabilities[OutcomeComplete] != 0.7 || len(outcome.Probabilities) != 4 {
				t.Errorf("outcome probabilities = %v, want all four options with complete at 0.7", outcome.Probabilities)
			}
		})
	}
}

// A branch merged with --no-ff into target has its head reachable from target.
// With real git, target..head holds no commits while the diff over the same
// range is not empty (target gained a commit after the merge), so readClone
// must report errNoCommits and not hand that diff on. A branch still ahead of
// target reads normally.
func TestJevSubmissionRealGitEmptyRangeIsLanded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := runGit(ctx, dir, append([]string{
			"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false",
		}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	commit := func(file, subject string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(subject+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", file)
		git("commit", "--quiet", "-m", subject)
	}
	git("init", "--quiet", "-b", "main")
	commit("base.txt", "base")
	git("checkout", "--quiet", "-b", "landed")
	commit("landed.txt", "fix: landed work")
	landedHead := git("rev-parse", "HEAD")
	git("checkout", "--quiet", "-b", "ahead", "main")
	commit("ahead.txt", "fix: still ahead")
	aheadHead := git("rev-parse", "HEAD")
	git("checkout", "--quiet", "main")
	git("merge", "--quiet", "--no-ff", "-m", "land landed", "landed")
	commit("later.txt", "feat: later work on main")

	p := &Pass{Git: &bareGit{dir: dir, timeout: time.Minute, run: runGit}}

	if stat, err := p.Git.Numstat(ctx, "main", landedHead); err != nil || !strings.Contains(stat, "later.txt") {
		t.Fatalf("numstat over the landed range = %q, %v; want the later change on main", stat, err)
	}
	_, _, err := p.readClone(ctx, land.Work{Target: "main", Head: landedHead})
	if !errors.Is(err, errNoCommits) || errors.Is(err, errUnreadable) {
		t.Errorf("readClone(landed) error = %v, want errNoCommits and not errUnreadable", err)
	}

	subjects, stat, err := p.readClone(ctx, land.Work{Target: "main", Head: aheadHead})
	if err != nil {
		t.Fatalf("readClone(ahead): %v", err)
	}
	if len(subjects) != 1 || subjects[0] != "fix: still ahead" || !strings.Contains(stat, "ahead.txt") {
		t.Errorf("readClone(ahead) = %v, %q; want its one commit and its diff", subjects, stat)
	}
}
