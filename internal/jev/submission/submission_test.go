package submission

import (
	"context"
	"encoding/json"
	"errors"
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

// fakeSubmissions returns the issues it holds and records the cursor it was
// asked from.
type fakeSubmissions struct {
	issues []*beads.Issue
	err    error
	since  []time.Time
}

func (f *fakeSubmissions) Since(t time.Time) ([]*beads.Issue, error) {
	f.since = append(f.since, t)
	return f.issues, f.err
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
			QWillPassReview:       0.8,
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
	if len(l.Answers) != 6 {
		t.Fatalf("answers = %+v, want one per question", l.Answers)
	}
	if n := l.Answers[QWillPassReview].Noul; n == nil || *n != 0.8 {
		t.Errorf("will_pass_review = %+v", l.Answers[QWillPassReview])
	}
	if a := l.Answers[QRisk]; a.Score == nil || *a.Score != 1 || a.Confidence == nil || *a.Confidence != 0.6 || a.Noul != nil {
		t.Errorf("risk = %+v, want score 1 at 0.6", a)
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
	want := "bead_acceptance bead_goal bead_title commit_subjects files_changed"
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

	// A first pass whose fetch fails writes no cursor, so the pass that
	// succeeds is still the one that starts it.
	_ = os.Remove(r.pass.CursorPath)
	if _, err := r.pass.Run(context.Background()); !errors.Is(err, ErrFetch) {
		t.Fatalf("err = %v, want ErrFetch", err)
	}
	if _, ok, _ := battery.ReadCursor(r.pass.CursorPath); ok {
		t.Error("a first pass whose fetch failed wrote a cursor")
	}
}

// When the clone cannot read a submission at all, it is skipped, counted, and
// the cursor moves past it so the pass is not stuck on it.
func TestJevSubmissionUnreadableSubmissionIsSkippedAndPassed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.submissions.issues = []*beads.Issue{r.ready("gt-a", time.Minute)}
	r.git.err = errors.New("bad object")

	sum, err := r.pass.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 0 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v, want 0 asked and 1 skipped", sum)
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
