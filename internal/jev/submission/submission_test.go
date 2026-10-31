package submission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

// t0 is the cursor every test starts from; submissions are stamped after it.
var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// fakeSubmissions returns the entries it holds, then each issue it holds as a
// waiting entry carrying the issue's READY TO LAND block, and records the
// cursor it was asked from.
type fakeSubmissions struct {
	issues  []*beads.Issue
	entries []Entry
	err     error
	since   []time.Time
}

func (f *fakeSubmissions) Since(t time.Time) ([]Entry, error) {
	f.since = append(f.since, t)
	out := append([]Entry(nil), f.entries...)
	for _, issue := range f.issues {
		w, ok := land.ParseReadyNote(issue.Notes)
		if !ok {
			continue
		}
		w.BeadID = issue.ID
		out = append(out, Entry{Issue: issue, Work: w, Seen: w.Submitted})
	}
	return out, f.err
}

// fakeGit returns scripted subjects and numstat per head and records every
// range it is asked for.
type fakeGit struct {
	syncErr  error
	subjects map[string][]string
	numstat  map[string]string
	err      error
	synced   int
	ranges   []string
}

func (f *fakeGit) Sync(context.Context) error {
	f.synced++
	return f.syncErr
}

func (f *fakeGit) CommitSubjects(_ context.Context, target, head string, _ int) ([]string, error) {
	f.ranges = append(f.ranges, target+".."+head)
	if f.err != nil {
		return nil, f.err
	}
	return f.subjects[head], nil
}

func (f *fakeGit) Numstat(_ context.Context, target, head string) (string, error) {
	f.ranges = append(f.ranges, target+".."+head)
	if f.err != nil {
		return "", f.err
	}
	return f.numstat[head], nil
}

// fakeAsker answers every Ask with the same answers, or fails with errs in
// order, and records every state and question set.
type fakeAsker struct {
	answers *jev.Answers
	errs    []error
	states  []string
	asked   []map[string]jev.Question
}

func (f *fakeAsker) Ask(_ context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	f.states = append(f.states, state.(string))
	f.asked = append(f.asked, qs)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	return f.answers, nil
}

func goodAnswers() *jev.Answers {
	return &jev.Answers{
		Model: "jev-1.13.0",
		Noul: map[string]float64{
			QTestsCoverChange:     0.7,
			QBeyondGoal:           0.1,
			QCommitsMatchChange:   0.9,
			QTouchesTownMachinery: 0.2,
			QClaimsVerified:       0.9,
			QVerificationApplies:  0.6,
		},
		Score: map[string]jev.ScoreAnswer{QRisk: {Score: 1, Confidence: 0.6}},
	}
}

// rig is a pass over fakes in a temp dir, with a cursor already set at t0.
type rig struct {
	pass        *Pass
	submissions *fakeSubmissions
	git         *fakeGit
	asker       *fakeAsker
	allows      func() (bool, error)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{
		submissions: &fakeSubmissions{},
		git:         &fakeGit{subjects: map[string][]string{}, numstat: map[string]string{}},
		asker:       &fakeAsker{answers: goodAnswers()},
		allows:      func() (bool, error) { return true, nil },
	}
	r.pass = &Pass{
		Asker:       r.asker,
		Submissions: r.submissions,
		Git:         r.git,
		LogPath:     filepath.Join(dir, "submission.jsonl"),
		CursorPath:  filepath.Join(dir, "submission.cursor.json"),
		Allows:      func() (bool, error) { return r.allows() },
		Now:         func() time.Time { return t0.Add(time.Hour) },
	}
	if err := battery.WriteCursor(r.pass.CursorPath, battery.Cursor{Time: t0}); err != nil {
		t.Fatal(err)
	}
	return r
}

// ready is a work bead submitted for landing at t0+after, with a READY TO LAND
// block and a changed file in its scripted diff.
func (r *rig) ready(bead string, after time.Duration) *beads.Issue {
	issue := &beads.Issue{
		ID:       bead,
		Title:    "bead " + bead,
		Status:   "open",
		Assignee: "gastown/polecats/slit",
		Notes: land.FormatReadyNote(land.Work{
			BeadID: bead, Branch: "polecat/opal/" + bead, Head: "head-" + bead,
			Target: "main", Worker: "opal", Submitted: t0.Add(after),
		}),
		Description: "## Goal\n\nDo the thing.\n\n## Acceptance\n\n- [ ] it works\n",
	}
	r.git.subjects["head-"+bead] = []string{"fix: do the thing (" + bead + ")"}
	r.git.numstat["head-"+bead] = "10\t2\tinternal/x.go\n"
	return issue
}

func readLines(t *testing.T, path string) []Line {
	t.Helper()
	lines, err := ReadRecent(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// Each submission after the cursor earns one line, one stamped before it
// earns none, and a second pass with nothing new appends nothing.
func TestJevSubmissionPassLogsOneLinePerNewSubmission(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.submissions.issues = []*beads.Issue{
		r.ready("gt-before", -time.Minute),
		r.ready("gt-a", time.Minute),
		r.ready("gt-c", 3*time.Minute),
	}

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 2 || sum.Skipped != 0 || sum.CapReached || sum.Started {
		t.Fatalf("summary = %+v, want 2 asked", sum)
	}
	if r.git.synced != 1 {
		t.Errorf("Synced %d times, want 1", r.git.synced)
	}
	if len(r.submissions.since) != 1 || !r.submissions.since[0].Equal(t0) {
		t.Errorf("listed since %v, want the cursor t0", r.submissions.since)
	}
	lines := readLines(t, r.pass.LogPath)
	if len(lines) != 2 || lines[0].Bead != "gt-a" || lines[1].Bead != "gt-c" {
		t.Fatalf("log lines = %+v, want gt-a then gt-c", lines)
	}

	sum, err = r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 0 || len(r.asker.states) != 2 {
		t.Errorf("second pass asked %d (total Asks %d), want nothing new", sum.Asked, len(r.asker.states))
	}
	if got := len(readLines(t, r.pass.LogPath)); got != 2 {
		t.Errorf("log holds %d lines after the second pass, want 2", got)
	}
}

// waitingEntry is the submission as its READY TO LAND block lists it, for a
// bead the landing worker has not written a first-sight record for yet.
func waitingEntry(t *testing.T, issue *beads.Issue) Entry {
	t.Helper()
	w, ok := land.ParseReadyNote(issue.Notes)
	if !ok {
		t.Fatalf("%s carries no READY TO LAND block", issue.ID)
	}
	w.BeadID = issue.ID
	return Entry{Issue: issue, Work: w, Seen: w.Submitted}
}

// recordEntry is the same submission as its first-sight record lists it, first
// seen at seen, with the commits the landing worker read.
func recordEntry(issue *beads.Issue, head string, seen time.Time) Entry {
	return Entry{
		Issue: issue,
		Work: land.Work{
			BeadID: issue.ID, Rig: RigName, Branch: "polecat/opal/" + issue.ID,
			Head: head, Target: "main", Submitted: seen.Add(-time.Minute),
		},
		Seen:     seen,
		Subjects: []string{"fix: a thing (" + issue.ID + ")"},
		Numstat:  "1\t0\tinternal/x.go\n",
	}
}

// logLine appends one submission-log line for bead at head at a question
// version, as an earlier pass would have.
func (r *rig) logLine(t *testing.T, bead, head string, version int) {
	t.Helper()
	if err := battery.Append(r.pass.LogPath, Line{TS: t0, Bead: bead, Head: head, QuestionVersion: version}); err != nil {
		t.Fatal(err)
	}
}

// A submission asked from its waiting bead is not asked again when the landing
// worker's first-sight record for it reaches the next pass: the record is the
// same bead at the same head, listed under a later seen time (gt-v419b).
func TestJevSubmissionPassDoesNotAskTwiceForOneSubmission(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	issue := r.ready("gt-a", time.Minute)

	// The landing worker has not reached the bead yet, so the pass lists it
	// from its READY TO LAND block.
	r.submissions.entries = []Entry{waitingEntry(t, issue)}
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 || sum.Repeated != 0 {
		t.Fatalf("first pass = %+v, want the waiting bead asked", sum)
	}

	// The record the landing worker wrote afterwards holds the same submission
	// (same bead, head and submitted stamp) and is seen past the cursor, so it
	// reaches this pass as a new submission.
	rec := waitingEntry(t, issue)
	rec.Seen = t0.Add(6 * time.Minute)
	rec.Subjects = []string{"fix: do the thing (gt-a)"}
	rec.Numstat = "10\t2\tinternal/x.go\n"
	r.submissions.entries = []Entry{rec}
	sum, err = r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 0 || sum.Repeated != 1 {
		t.Fatalf("second pass = %+v, want the record passed over as repeated", sum)
	}
	if len(r.asker.states) != 1 {
		t.Errorf("Jev was asked %d times, want once", len(r.asker.states))
	}
	lines := readLines(t, r.pass.LogPath)
	if len(lines) != 1 || lines[0].Bead != "gt-a" || lines[0].Head != "head-gt-a" {
		t.Fatalf("log lines = %+v, want one line for gt-a at head-gt-a", lines)
	}
}

// A line at the current question version keeps the pass from asking that bead
// at that head again; the same bead at a new head is a new submission and is
// asked, and a line at an older question version does not block.
func TestJevSubmissionRepeatedSubmissionIsSkipped(t *testing.T) {
	t.Parallel()
	t.Run("the same head at the current version is skipped", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		issue := r.ready("gt-a", time.Minute)
		r.logLine(t, "gt-a", "head-gt-a", Version)
		r.submissions.entries = []Entry{
			recordEntry(issue, "head-gt-a", t0.Add(2*time.Minute)),
			recordEntry(issue, "head-gt-a2", t0.Add(3*time.Minute)),
		}

		sum, err := r.pass.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if sum.Asked != 1 || sum.Repeated != 1 {
			t.Fatalf("summary = %+v, want the logged head repeated and the new head asked", sum)
		}
		lines := readLines(t, r.pass.LogPath)
		if len(lines) != 2 || lines[1].Head != "head-gt-a2" {
			t.Fatalf("log lines = %+v, want the seeded line then head-gt-a2", lines)
		}
	})

	t.Run("a line at an older version does not block", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		issue := r.ready("gt-b", time.Minute)
		r.logLine(t, "gt-b", "head-gt-b", Version-1)
		r.submissions.entries = []Entry{recordEntry(issue, "head-gt-b", t0.Add(2*time.Minute))}

		sum, err := r.pass.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if sum.Asked != 1 || sum.Repeated != 0 {
			t.Fatalf("summary = %+v, want the submission asked under the current version", sum)
		}
		lines := readLines(t, r.pass.LogPath)
		if len(lines) != 2 || lines[1].Head != "head-gt-b" {
			t.Fatalf("log lines = %+v, want the seeded line then head-gt-b", lines)
		}
	})
}

// A pass that cannot read today's spend stops before it asks, and the cursor
// stays where it was.
func TestJevSubmissionSpendReadErrorStopsThePass(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.submissions.issues = []*beads.Issue{r.ready("gt-a", time.Minute)}
	r.allows = func() (bool, error) { return false, errors.New("ledger unreadable") }

	sum, err := r.pass.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reading today's spend") {
		t.Fatalf("err = %v, want the spend read named", err)
	}
	if sum.Asked != 0 || len(r.asker.states) != 0 {
		t.Errorf("a pass with no spend reading asked %d submissions", sum.Asked)
	}
	cur, ok, err := battery.ReadCursor(r.pass.CursorPath)
	if err != nil || !ok || !cur.Time.Equal(t0) {
		t.Errorf("cursor = %+v ok=%v err=%v, want it left at t0", cur, ok, err)
	}
}

// A failed call that is not an unusable response stops the pass, so the
// submission waits for the next pass rather than passing for an answer.
func TestJevSubmissionCallFailureStopsThePass(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.submissions.issues = []*beads.Issue{r.ready("gt-a", time.Minute), r.ready("gt-b", 2*time.Minute)}
	r.asker.errs = []error{errors.New("connection reset")}

	if _, err := r.pass.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v, want the call's failure returned", err)
	}
	if _, statErr := os.Stat(r.pass.LogPath); !os.IsNotExist(statErr) {
		t.Errorf("a pass stopped by a failed call wrote a log line: %v", statErr)
	}
	cur, ok, _ := battery.ReadCursor(r.pass.CursorPath)
	if !ok || !cur.Time.Equal(t0) {
		t.Errorf("cursor = %+v ok=%v, want it left at t0 so the next pass asks gt-a again", cur, ok)
	}
}

// A log the pass cannot append to stops it with the cursor unmoved, so a
// submission whose answer was never written is asked again rather than
// missing from the log that scores it.
func TestJevSubmissionAppendFailureStopsThePass(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// The log is a link into a directory that does not exist: reading it reads
	// as a missing log, and the append cannot create the file behind it.
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone", "submission.jsonl"), r.pass.LogPath); err != nil {
		t.Fatal(err)
	}
	r.submissions.issues = []*beads.Issue{r.ready("gt-a", time.Minute)}

	if _, err := r.pass.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "creating the log") {
		t.Fatalf("err = %v, want the failed append returned", err)
	}
	cur, ok, _ := battery.ReadCursor(r.pass.CursorPath)
	if !ok || !cur.Time.Equal(t0) {
		t.Errorf("cursor = %+v ok=%v, want it left at t0 so the next pass asks gt-a again", cur, ok)
	}
}

// A log the pass cannot read stops it before it asks anything: without the
// lines already there, the pass cannot tell a submission it has asked from a
// new one, so asking could repeat a whole pass's worth of calls.
func TestJevSubmissionUnreadableLogStopsThePass(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// A directory where the log belongs opens, and then fails to read.
	if err := os.Mkdir(r.pass.LogPath, 0o700); err != nil {
		t.Fatal(err)
	}
	r.submissions.issues = []*beads.Issue{r.ready("gt-a", time.Minute)}

	sum, err := r.pass.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reading the submission log") {
		t.Fatalf("err = %v, want the unreadable log named", err)
	}
	if sum.Asked != 0 || len(r.asker.states) != 0 {
		t.Errorf("a pass with no readable log asked %d submissions", sum.Asked)
	}
	cur, ok, err := battery.ReadCursor(r.pass.CursorPath)
	if err != nil || !ok || !cur.Time.Equal(t0) {
		t.Errorf("cursor = %+v ok=%v err=%v, want it left at t0", cur, ok, err)
	}
}

// A pass with nothing waiting has no submission to decide, so it does not read
// the log: an idle pass stays cheap however long the log grows (gt-ipgpu).
func TestJevSubmissionIdlePassDoesNotReadTheLog(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// A directory where the log belongs cannot be read, and nothing is waiting.
	if err := os.Mkdir(r.pass.LogPath, 0o700); err != nil {
		t.Fatal(err)
	}

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatalf("idle pass err = %v, want no read of the log", err)
	}
	if sum != (Summary{}) || len(r.asker.states) != 0 {
		t.Errorf("idle pass summary = %+v, asked %d; want nothing done", sum, len(r.asker.states))
	}
}

// The summary counts a submission the log already holds apart from one the
// pass could not read, so the daemon line reports what each cost.
func TestJevSubmissionSummaryCountsRepeated(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	old := r.ready("gt-old", time.Minute)
	bad := r.ready("gt-bad", 2*time.Minute)
	fresh := r.ready("gt-new", 3*time.Minute)
	r.logLine(t, "gt-old", "head-gt-old", Version)
	r.git.err = errors.New("bad object")
	badEntry := recordEntry(bad, "head-gt-bad", t0.Add(3*time.Minute))
	badEntry.Subjects = nil
	r.submissions.entries = []Entry{
		recordEntry(old, "head-gt-old", t0.Add(2*time.Minute)),
		badEntry,
		recordEntry(fresh, "head-gt-new", t0.Add(4*time.Minute)),
	}

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 || sum.Skipped != 1 || sum.Repeated != 1 {
		t.Fatalf("summary = %+v, want 1 asked, 1 skipped and 1 repeated", sum)
	}
	lines := readLines(t, r.pass.LogPath)
	if len(lines) != 2 || lines[1].Bead != "gt-new" {
		t.Fatalf("log lines = %+v, want the seeded line then gt-new", lines)
	}
}

// The line holds exactly the documented keys, with the submission's own
// fields, the model the API returned and the question version.
func TestJevSubmissionLineShape(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	issue := r.ready("gt-a", time.Minute)
	issue.Labels = []string{"agent:claude-haiku"}
	r.submissions.issues = []*beads.Issue{issue}

	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(r.pass.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := "agent answers bead branch head model question_version submitted target ts"
	if strings.Join(keys, " ") != want {
		t.Errorf("keys = %v, want %s", keys, want)
	}

	l := readLines(t, r.pass.LogPath)[0]
	if l.TS != t0.Add(time.Hour) || l.Bead != "gt-a" || l.Branch != "polecat/opal/gt-a" ||
		l.Head != "head-gt-a" || l.Target != "main" || !l.Submitted.Equal(t0.Add(time.Minute)) ||
		l.Agent != "claude-haiku" || l.QuestionVersion != Version || l.Model != "jev-1.13.0" {
		t.Errorf("line = %+v", l)
	}
	if len(l.Answers) != 7 {
		t.Fatalf("answers = %+v, want one per question", l.Answers)
	}
	if a := l.Answers[QRisk]; a.Score == nil || *a.Score != 1 || a.Confidence == nil || *a.Confidence != 0.6 || a.Noul != nil {
		t.Errorf("risk = %+v, want score 1 at 0.6", a)
	}
	if a := l.Answers[QClaimsVerified]; a.Noul == nil || *a.Noul != 0.9 {
		t.Errorf("claims_verified = %+v", a)
	}
	for _, id := range []string{QWillPassReview, QBoxesCredible, QOutcome} {
		if a, ok := l.Answers[id]; ok {
			t.Errorf("%s = %+v, want no answer: the question is no longer asked", id, a)
		}
	}
	for _, leak := range []string{"Do the thing.", "internal/x.go", "@@"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("log line carries state or diff text %q: %s", leak, raw)
		}
	}
}

// stateFields parses a state into field name -> raw JSON value, failing on a
// line that is not "name: <json>".
func stateFields(t *testing.T, state string) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	for _, l := range strings.Split(strings.TrimSuffix(state, "\n"), "\n") {
		name, val, ok := strings.Cut(l, ": ")
		if !ok {
			t.Fatalf("state line %q is not 'name: value'", l)
		}
		if !json.Valid([]byte(val)) {
			t.Fatalf("state value for %s is not JSON: %s", name, val)
		}
		out[name] = json.RawMessage(val)
	}
	return out
}

// The state holds the listed fields and no others, each JSON-quoted on one
// line, and no diff or bead text outside them.
func TestJevSubmissionStateHoldsOnlyTheListedFields(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	issue := r.ready("gt-a", time.Minute)
	issue.Title = "Fix <b>& the thing"
	issue.Description = "attached_molecule: gt-wisp-1\nagent=claude-haiku\n\n" +
		"## Goal\n\nMake it so.\n\n## Constraints\n\nsecret-constraint text\n\n" +
		"## Acceptance\n\n- [ ] it works\n- [ ] it still works\n"
	r.git.numstat["head-gt-a"] = "12\t3\tinternal/x.go\n" +
		"-\t-\tdocs/logo.png\n" +
		"@@ -1,2 +1,2 @@\n+diff-hunk-secret\n" +
		"4\t0\tinternal/x_test.go\n"
	r.submissions.issues = []*beads.Issue{issue}

	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := r.asker.states[0]
	fields := stateFields(t, state)

	var names []string
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	want := "bead_acceptance bead_goal bead_title commit_subjects files_changed mr_notes"
	if strings.Join(names, " ") != want {
		t.Errorf("fields = %v, want %s", names, want)
	}
	for _, leak := range []string{"diff-hunk-secret", "@@", "secret-constraint", "gt-wisp-1"} {
		if strings.Contains(state, leak) {
			t.Errorf("state leaks %q:\n%s", leak, state)
		}
	}

	var title, goal, acceptance string
	var subjects []string
	var files []fileChange
	for name, into := range map[string]any{
		"bead_title": &title, "bead_goal": &goal, "bead_acceptance": &acceptance,
		"commit_subjects": &subjects, "files_changed": &files,
	} {
		if err := json.Unmarshal(fields[name], into); err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
	}
	if title != "Fix <b>& the thing" {
		t.Errorf("bead_title = %q", title)
	}
	if goal != "Make it so." {
		t.Errorf("bead_goal = %q", goal)
	}
	if acceptance != "- [ ] it works\n- [ ] it still works" {
		t.Errorf("bead_acceptance = %q", acceptance)
	}
	if len(subjects) != 1 || subjects[0] != "fix: do the thing (gt-a)" {
		t.Errorf("commit_subjects = %v", subjects)
	}
	wantFiles := []fileChange{{Path: "internal/x.go", Added: 12, Removed: 3}, {Path: "docs/logo.png"}, {Path: "internal/x_test.go", Added: 4}}
	if len(files) != len(wantFiles) {
		t.Fatalf("files_changed = %+v, want %+v", files, wantFiles)
	}
	for i := range wantFiles {
		if files[i] != wantFiles[i] {
			t.Errorf("files_changed[%d] = %+v, want %+v", i, files[i], wantFiles[i])
		}
	}
}

// The state cuts each section to 1500 runes, lists at most 20 commit subjects
// and at most 80 files, however much the submission carries.
func TestJevSubmissionStateCapsSectionsSubjectsAndFiles(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	issue := r.ready("gt-a", time.Minute)
	long := strings.Repeat("g", 2000)
	issue.Description = "## Goal\n\n" + long + "\n\n## Acceptance\n\n" + long + "\n"
	subjects := make([]string, 25)
	for i := range subjects {
		subjects[i] = "commit"
	}
	r.git.subjects["head-gt-a"] = subjects
	var stat strings.Builder
	for i := 0; i < 100; i++ {
		stat.WriteString("1\t0\tfile")
		stat.WriteString(string(rune('a' + i%26)))
		stat.WriteString(".go\n")
	}
	r.git.numstat["head-gt-a"] = stat.String()
	r.submissions.issues = []*beads.Issue{issue}

	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	fields := stateFields(t, r.asker.states[0])
	var goal, acceptance string
	var gotSubjects []string
	var files []fileChange
	for name, into := range map[string]any{
		"bead_goal": &goal, "bead_acceptance": &acceptance,
		"commit_subjects": &gotSubjects, "files_changed": &files,
	} {
		if err := json.Unmarshal(fields[name], into); err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
	}
	if n := len([]rune(goal)); n != maxSectionRunes+1 {
		t.Errorf("bead_goal is %d runes, want %d and an ellipsis", n, maxSectionRunes)
	}
	if n := len([]rune(acceptance)); n != maxSectionRunes+1 {
		t.Errorf("bead_acceptance is %d runes, want %d and an ellipsis", n, maxSectionRunes)
	}
	if len(gotSubjects) != maxSubjects {
		t.Errorf("commit_subjects holds %d, want %d", len(gotSubjects), maxSubjects)
	}
	if len(files) != maxFiles {
		t.Errorf("files_changed holds %d, want %d", len(files), maxFiles)
	}
}

// The MR notes the state quotes start at the live READY TO LAND block: an
// earlier attempt's rejection stays out, and the notes after the block reach
// the state.
func TestJevSubmissionMRNotesStartAtTheLiveReadyBlock(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	issue := r.ready("gt-a", time.Minute)
	earlier := land.FormatRejectionNote(land.RejectionNote{
		Attempt: 1, Reason: "earlier-rejection-reason", Branch: "b", Target: "main", MR: "gt-a",
	})
	issue.Notes = earlier + "\n" + issue.Notes + "\nverification: go test ./internal/x passed"
	r.submissions.issues = []*beads.Issue{issue}

	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var notes string
	if err := json.Unmarshal(stateFields(t, r.asker.states[0])["mr_notes"], &notes); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(notes, land.ReadyNoteMarker+"\n") {
		t.Errorf("mr_notes = %q, want it to start at the READY TO LAND block", notes)
	}
	if strings.Contains(notes, "earlier-rejection-reason") {
		t.Errorf("mr_notes carries an earlier attempt's rejection: %q", notes)
	}
	if !strings.HasSuffix(notes, "verification: go test ./internal/x passed") {
		t.Errorf("mr_notes = %q, want the notes after the block", notes)
	}
}

// The MR notes are cut to 4000 runes, the one cap on the state's free text.
func TestJevSubmissionMRNotesAreCapped(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	issue := r.ready("gt-a", time.Minute)
	issue.Notes += "\n" + strings.Repeat("n", 2*maxNotesRunes)
	r.submissions.issues = []*beads.Issue{issue}

	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var notes string
	if err := json.Unmarshal(stateFields(t, r.asker.states[0])["mr_notes"], &notes); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(notes)); n != maxNotesRunes+1 {
		t.Errorf("mr_notes is %d runes, want %d and an ellipsis", n, maxNotesRunes)
	}
}

// The first pass only starts the cursor: it asks about nothing, so a new town
// does not pay to ask about every submission already waiting.
func TestJevSubmissionFirstPassOnlySetsTheCursor(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	_ = os.Remove(r.pass.CursorPath)
	r.submissions.issues = []*beads.Issue{r.ready("gt-a", -time.Minute)}

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Started || sum.Asked != 0 || len(r.asker.states) != 0 {
		t.Fatalf("summary = %+v, want a started pass that asked nothing", sum)
	}
	cur, ok, err := battery.ReadCursor(r.pass.CursorPath)
	if err != nil || !ok || !cur.Time.Equal(t0.Add(time.Hour)) {
		t.Errorf("cursor = %+v ok=%v err=%v, want the pass's own time", cur, ok, err)
	}
}

// A clone that cannot be fetched skips the whole pass, and the cursor does not
// move, so the submissions behind it are asked by the next pass.
func TestJevSubmissionFetchFailureLeavesTheCursorUnmoved(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.git.syncErr = errors.New("could not resolve host")
	r.submissions.issues = []*beads.Issue{r.ready("gt-a", time.Minute)}

	_, err := r.pass.Run(context.Background())
	if !errors.Is(err, ErrFetch) {
		t.Fatalf("err = %v, want ErrFetch", err)
	}
	if len(r.asker.states) != 0 {
		t.Errorf("a pass whose fetch failed made %d Jev calls", len(r.asker.states))
	}
	cur, ok, err := battery.ReadCursor(r.pass.CursorPath)
	if err != nil || !ok || !cur.Time.Equal(t0) {
		t.Errorf("cursor = %+v ok=%v err=%v, want it at t0", cur, ok, err)
	}

	// A first pass asks nothing, so it needs no clone and a failing fetch does
	// not stop it from starting.
	_ = os.Remove(r.pass.CursorPath)
	synced := r.git.synced
	sum, err := r.pass.Run(context.Background())
	if err != nil || !sum.Started {
		t.Fatalf("first pass = %+v, %v; want it started with no fetch", sum, err)
	}
	if r.git.synced != synced {
		t.Errorf("a first pass fetched the clone %d more times, want none", r.git.synced-synced)
	}
}

// When the clone cannot read a submission at all, it is skipped, counted, and
// the cursor moves past it so the pass is not stuck on it.
func TestJevSubmissionUnreadableSubmissionIsSkippedAndPassed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.submissions.issues = []*beads.Issue{r.ready("gt-a", time.Minute)}
	r.git.err = errors.New("bad object")
	var warnings strings.Builder
	r.pass.Warnings = &warnings

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 0 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v, want 0 asked and 1 skipped", sum)
	}
	if got := warnings.String(); !strings.Contains(got, "gt-a") || !strings.Contains(got, "bad object") {
		t.Errorf("warnings = %q, want the skipped bead and the git error", got)
	}
	if _, err := os.Stat(r.pass.LogPath); !os.IsNotExist(err) {
		t.Errorf("a skipped submission wrote a log: %v", err)
	}
	cur, ok, _ := battery.ReadCursor(r.pass.CursorPath)
	if !ok || !cur.Time.Equal(t0.Add(time.Minute)) || cur.ID != "gt-a" {
		t.Errorf("cursor = %+v ok=%v, want it past gt-a", cur, ok)
	}
}

// When the cap says the day's spend is used up, the pass asks nothing and
// says so; the submissions wait for the next pass.
func TestJevSubmissionCapStopsThePass(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.submissions.issues = []*beads.Issue{r.ready("gt-a", time.Minute)}
	r.allows = func() (bool, error) { return false, nil }

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.CapReached || sum.Asked != 0 || len(r.asker.states) != 0 {
		t.Fatalf("summary = %+v, want the cap reached with nothing asked", sum)
	}
	cur, _, _ := battery.ReadCursor(r.pass.CursorPath)
	if !cur.Time.Equal(t0) {
		t.Errorf("a capped pass moved the cursor to %v", cur.Time)
	}
}

// A backlog larger than the window drains through Run over several passes, each
// taking at most backlogLimit submissions, and a day-cap stop partway through one
// leaves the rest for the next. Two waiting beads sit in the backlog: one whose
// record is the newest, and one with no record whose READY TO LAND block is
// stamped last. Each submission is asked exactly once, those two included, and
// the cursor ends past the last of them.
func TestRunDrainsABacklogLargerThanTheWindowOverSeveralPasses(t *testing.T) {
	t.Parallel()
	const backlog = 2*backlogLimit + 300
	const capStop = 100

	r := newRig(t)
	records := filepath.Join(t.TempDir(), land.SubmissionsSeenFile)
	known := map[string]*beads.Issue{}
	record := func(id string, at time.Time) {
		t.Helper()
		rec := land.SubmissionRecord{
			SeenAt: at, Bead: id, Rig: RigName,
			Branch: "polecat/opal/" + id, Head: "head-" + id, Target: "main",
			Subjects: []string{"fix: do the thing (" + id + ")"}, Numstat: "10\t2\tinternal/x.go\n",
		}
		if err := battery.Append(records, rec); err != nil {
			t.Fatal(err)
		}
	}
	for i := range backlog {
		id := fmt.Sprintf("gt-new-%d", i)
		record(id, t0.Add(time.Duration(i+1)*time.Second))
		known[id] = &beads.Issue{ID: id, Status: "closed"}
	}

	// gt-held waits at the head its record names. Its block was stamped first,
	// so a pass that listed it from the bead would ask it before its record's
	// turn, and then ask it again at the record.
	last := t0.Add(time.Duration(backlog+1) * time.Second)
	held := &beads.Issue{ID: "gt-held", Status: "open", Labels: []string{land.LabelReadyToLand}, Notes: land.FormatReadyNote(land.Work{
		Branch: "polecat/opal/gt-held", Head: "head-gt-held", Target: "main", Submitted: t0.Add(time.Second),
	})}
	record("gt-held", last)
	known["gt-held"] = held
	// gt-late has no record, and its block is stamped after every record.
	late := &beads.Issue{ID: "gt-late", Status: "open", Labels: []string{land.LabelReadyToLand}, Notes: land.FormatReadyNote(land.Work{
		Branch: "polecat/opal/gt-late", Head: "head-gt-late", Target: "main", Submitted: last.Add(time.Hour),
	})}
	r.git.subjects["head-gt-late"] = []string{"fix: do the other thing (gt-late)"}
	r.pass.Submissions = rigBeads{b: &recordingLister{waiting: []*beads.Issue{held, late}, byID: known}, records: records}

	// The first pass stops at the day's cap after capStop asks.
	checks := 0
	r.pass.Allows = func() (bool, error) {
		checks++
		return checks <= capStop, nil
	}
	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatalf("capped pass: %v", err)
	}
	if !sum.CapReached || sum.Asked != capStop {
		t.Fatalf("capped pass = %+v, want the cap reached after %d asks", sum, capStop)
	}

	r.pass.Allows = func() (bool, error) { return true, nil }
	total := capStop
	for pass := 2; ; pass++ {
		if pass > 10 {
			t.Fatal("the backlog was still not drained after 10 passes")
		}
		sum, err := r.pass.Run(context.Background())
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if sum.Asked > backlogLimit+2 {
			t.Fatalf("pass %d asked %d, want at most backlogLimit (%d) plus the two waiting beads", pass, sum.Asked, backlogLimit)
		}
		if sum.Asked == 0 {
			break
		}
		total += sum.Asked
	}

	lines, err := ReadRecent(r.pass.LogPath, 2*backlog)
	if err != nil {
		t.Fatal(err)
	}
	asked := map[string]int{}
	for _, l := range lines {
		asked[l.Bead]++
	}
	for id, n := range asked {
		if n != 1 {
			t.Errorf("%s asked %d times, want once", id, n)
		}
	}
	if want := backlog + 2; len(asked) != want || total != want {
		t.Fatalf("log holds %d submissions and the passes asked %d, want %d of each", len(asked), total, want)
	}
	for _, id := range []string{"gt-held", "gt-late"} {
		if asked[id] != 1 {
			t.Errorf("waiting bead %s asked %d times, want once", id, asked[id])
		}
	}
	cur, _, err := battery.ReadCursor(r.pass.CursorPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := last.Add(time.Hour); !cur.Time.Equal(want) {
		t.Fatalf("cursor = %v, want it past the last submission at %v", cur.Time, want)
	}
}

// An unusable answer is skipped, not logged, and the next submission is still
// asked.
func TestJevSubmissionBadResponseIsSkipped(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.submissions.issues = []*beads.Issue{r.ready("gt-a", time.Minute), r.ready("gt-b", 2*time.Minute)}
	r.asker.errs = []error{jev.ErrBadResponse}

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v, want 1 asked and 1 skipped", sum)
	}
	lines := readLines(t, r.pass.LogPath)
	if len(lines) != 1 || lines[0].Bead != "gt-b" {
		t.Errorf("log lines = %+v, want only gt-b", lines)
	}
}

// Another pass holding the run lock makes this one step aside.
func TestJevSubmissionRunStepsAsideWhenTheLockIsHeld(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	release, err := battery.Lock(r.pass.CursorPath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := r.pass.Run(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if r.git.synced != 0 {
		t.Errorf("a pass that could not lock fetched the clone")
	}
}

// A bead resubmitted after a rework has two READY TO LAND blocks; the pass
// asks about the last one, at its own time.
func TestJevSubmissionAsksAboutTheLastReadyBlock(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	issue := r.ready("gt-a", time.Minute)
	issue.Notes = land.FormatReadyNote(land.Work{
		Branch: "old", Head: "head-old", Target: "main", Submitted: t0.Add(-time.Hour),
	}) + "\n" + issue.Notes
	r.git.subjects["head-gt-a"] = []string{"fix: again (gt-a)"}
	r.submissions.issues = []*beads.Issue{issue}

	if _, err := r.pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, r.pass.LogPath)
	if len(lines) != 1 || lines[0].Head != "head-gt-a" || !lines[0].Submitted.Equal(t0.Add(time.Minute)) {
		t.Fatalf("log lines = %+v, want the newer block", lines)
	}
}

// A submission whose record holds its commits is asked from them. The clone is
// never read or fetched, so a branch the landing pruned still earns its line.
func TestJevSubmissionAsksFromTheRecordWhenTheCloneCannotRead(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	issue := r.ready("gt-a", time.Minute)
	delete(r.git.subjects, "head-gt-a")
	r.git.syncErr = errors.New("could not resolve host")
	r.git.err = errors.New("bad object")
	r.submissions.entries = []Entry{{
		Issue: issue,
		Work: land.Work{
			BeadID: "gt-a", Branch: "polecat/opal/gt-a", Head: "head-gt-a", Target: "main",
			Submitted: t0.Add(time.Minute),
		},
		Seen:     t0.Add(2 * time.Minute),
		Subjects: []string{"fix: do the thing (gt-a)"},
		Numstat:  "10\t2\tinternal/x.go",
	}}

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v, want the record asked", sum)
	}
	if r.git.synced != 0 || len(r.git.ranges) != 0 {
		t.Errorf("a record with subjects read the clone: synced %d, ranges %v", r.git.synced, r.git.ranges)
	}
	state := r.asker.states[0]
	if !strings.Contains(state, `commit_subjects: ["fix: do the thing (gt-a)"]`) || !strings.Contains(state, `"internal/x.go"`) {
		t.Errorf("state = %q, want the record's subjects and numstat", state)
	}
	cur, _, _ := battery.ReadCursor(r.pass.CursorPath)
	if !cur.Time.Equal(t0.Add(2*time.Minute)) || cur.ID != "gt-a" {
		t.Errorf("cursor = %+v, want it at the record's seen time", cur)
	}
}

// A record that holds no subjects, because the landing worker's clone lacked
// the commits, is asked from the pass's own clone, which is synced first.
func TestJevSubmissionRecordWithoutSubjectsFallsBackToTheClone(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	issue := r.ready("gt-a", time.Minute)
	r.submissions.entries = []Entry{{
		Issue: issue,
		Work: land.Work{
			BeadID: "gt-a", Branch: "polecat/opal/gt-a", Head: "head-gt-a", Target: "main",
			Submitted: t0.Add(time.Minute),
		},
		Seen: t0.Add(2 * time.Minute),
	}}

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 || r.git.synced != 1 {
		t.Fatalf("summary = %+v, synced %d times; want the clone read once and the entry asked", sum, r.git.synced)
	}
	if !strings.Contains(r.asker.states[0], `"fix: do the thing (gt-a)"`) {
		t.Errorf("state = %q, want the clone's subject", r.asker.states[0])
	}
}

// A bead that wears more than one agent: label has no agent in the record,
// because the dispatcher seats no such bead. A bead with no agent: label still
// takes the agent its attached vars name.
func TestAgentOfRefusesSeveralAgentLabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels []string
		desc   string
		want   string
	}{
		{name: "several labels", labels: []string{"agent:flint", "agent:opal"}, want: ""},
		{name: "several labels beat the attached var", labels: []string{"agent:flint", "agent:opal"}, desc: `attached_vars: ["agent=kestrel"]`, want: ""},
		{name: "one label", labels: []string{"agent:flint"}, want: "flint"},
		{name: "no label takes the attached var", desc: `attached_vars: ["agent=kestrel"]`, want: "kestrel"},
		// The dispatcher reads agent: labels trimmed and case-insensitively, so
		// the record names the agent it seats.
		{name: "label prefix in capitals", labels: []string{"AGENT:flint"}, want: "flint"},
		{name: "label with surrounding space", labels: []string{" agent:flint "}, want: "flint"},
		{name: "label value padded", labels: []string{"agent: flint "}, want: "flint"},
		{name: "blank label value names none", labels: []string{"agent:  "}, desc: `attached_vars: ["agent=kestrel"]`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := &beads.Issue{ID: "gt-a", Labels: tc.labels, Description: tc.desc}
			if got := agentOf(issue); got != tc.want {
				t.Errorf("agentOf(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}
