package land

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// fakeSubmissionGit answers the clone reads from scripted commits and stat, and
// records every head it is asked about.
type fakeSubmissionGit struct {
	messages map[string][]git.CommitMessage
	numstat  map[string]string
	err      error
	numErr   error
	logged   []string
}

func (f *fakeSubmissionGit) CommitMessages(base, head string) ([]git.CommitMessage, error) {
	f.logged = append(f.logged, "messages "+base+".."+head)
	if f.err != nil {
		return nil, f.err
	}
	return f.messages[head], nil
}

func (f *fakeSubmissionGit) DiffNumstat(base, head string) (string, error) {
	f.logged = append(f.logged, "numstat "+base+".."+head)
	if f.numErr != nil {
		return "", f.numErr
	}
	return f.numstat[head], nil
}

var recordAt = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

func recordWork(bead, head string) Work {
	return Work{
		BeadID: bead, Rig: "gastown", Branch: "polecat/opal/" + bead, Head: head,
		Target: "main", Worker: "opal", Submitted: recordAt.Add(-time.Minute),
	}
}

func newRecorder(t *testing.T, g SubmissionGit) (*SubmissionRecorder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), SubmissionsSeenFile)
	return &SubmissionRecorder{Path: path, Git: g, Now: func() time.Time { return recordAt }}, path
}

func readRecords(t *testing.T, path string) []SubmissionRecord {
	t.Helper()
	recs, err := ReadSubmissionRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// The worker writes one first-sight record per submission it has not recorded,
// and none for one already recorded, at the same bead and head.
func TestSubmissionRecorderWritesOneRecordPerNewSubmission(t *testing.T) {
	t.Parallel()
	g := &fakeSubmissionGit{
		messages: map[string][]git.CommitMessage{"h1": {{SHA: "a", Message: "fix: it (gt-a)\n\nbody"}}},
		numstat:  map[string]string{"h1": "3\t1\tinternal/x.go\n"},
	}
	rec, path := newRecorder(t, g)

	wrote, err := rec.Record(recordWork("gt-a", "h1"))
	if err != nil || !wrote {
		t.Fatalf("first sight = %v, %v; want a record written", wrote, err)
	}
	wrote, err = rec.Record(recordWork("gt-a", "h1"))
	if err != nil || wrote {
		t.Fatalf("second sight = %v, %v; want no record for one already recorded", wrote, err)
	}
	if wrote, err = rec.Record(recordWork("gt-a", "h2")); err != nil || !wrote {
		t.Fatalf("new head = %v, %v; want a record for the new head", wrote, err)
	}

	recs := readRecords(t, path)
	if len(recs) != 2 {
		t.Fatalf("log holds %d records, want 2 (one per bead and head)", len(recs))
	}
	if len(g.logged) == 0 || g.logged[0] != "messages origin/main..h1" {
		t.Errorf("first clone read = %v, want the commits between origin/main and the head", g.logged)
	}
	got := recs[0]
	if !got.SeenAt.Equal(recordAt) || got.Bead != "gt-a" || got.Rig != "gastown" || got.Branch != "polecat/opal/gt-a" ||
		got.Head != "h1" || got.Target != "main" || !got.Submitted.Equal(recordAt.Add(-time.Minute)) {
		t.Errorf("record = %+v, want the submission's own fields and the worker's clock", got)
	}
	if len(got.Subjects) != 1 || got.Subjects[0] != "fix: it (gt-a)" {
		t.Errorf("subjects = %q, want the subject line of each commit", got.Subjects)
	}
	if got.Numstat != "3\t1\tinternal/x.go" {
		t.Errorf("numstat = %q, want the diff's numstat without its trailing newline", got.Numstat)
	}
}

// A worker that restarts reads the log and writes nothing for a submission the
// log already holds, without reading the clone for it.
func TestSubmissionRecorderSkipsARecordedSubmissionAfterRestart(t *testing.T) {
	t.Parallel()
	g := &fakeSubmissionGit{messages: map[string][]git.CommitMessage{"h1": {{Message: "fix: a"}}}}
	first, path := newRecorder(t, g)
	if _, err := first.Record(recordWork("gt-a", "h1")); err != nil {
		t.Fatal(err)
	}

	g.logged = nil
	again := &SubmissionRecorder{Path: path, Git: g}
	wrote, err := again.Record(recordWork("gt-a", "h1"))
	if err != nil || wrote {
		t.Fatalf("after restart = %v, %v; want no record", wrote, err)
	}
	if len(g.logged) != 0 {
		t.Errorf("a recorded submission read the clone: %v", g.logged)
	}
	if n := len(readRecords(t, path)); n != 1 {
		t.Errorf("log holds %d records, want 1", n)
	}
}

// A clone that lacks the commits writes the record with no subjects and no
// stat, so the battery reads its own clone for that submission.
func TestSubmissionRecorderWritesARecordWithoutSubjectsWhenTheCloneLacksTheCommits(t *testing.T) {
	t.Parallel()
	g := &fakeSubmissionGit{err: errors.New("bad revision 'h1'")}
	rec, path := newRecorder(t, g)

	if wrote, err := rec.Record(recordWork("gt-a", "h1")); err != nil || !wrote {
		t.Fatalf("Record = %v, %v; want a record written despite the failed read", wrote, err)
	}
	recs := readRecords(t, path)
	if len(recs) != 1 || len(recs[0].Subjects) != 0 || recs[0].Numstat != "" {
		t.Fatalf("records = %+v, want one record with no subjects or stat", recs)
	}
	for _, call := range g.logged {
		if strings.HasPrefix(call, "numstat") {
			t.Errorf("the stat was read after the subjects failed: %v", g.logged)
		}
	}
}

// A record holds at most 20 subjects and 80 numstat lines, the same caps the
// battery puts on its state.
func TestSubmissionRecorderCapsSubjectsAndNumstat(t *testing.T) {
	t.Parallel()
	var msgs []git.CommitMessage
	var stat []string
	for i := range 25 {
		msgs = append(msgs, git.CommitMessage{Message: fmt.Sprintf("commit %d\n\nbody", i)})
		stat = append(stat, fmt.Sprintf("1\t0\tfile%d.go", i))
	}
	for i := 25; i < 100; i++ {
		stat = append(stat, fmt.Sprintf("1\t0\tfile%d.go", i))
	}
	g := &fakeSubmissionGit{
		messages: map[string][]git.CommitMessage{"h1": msgs},
		numstat:  map[string]string{"h1": strings.Join(stat, "\n") + "\n"},
	}
	rec, path := newRecorder(t, g)
	if _, err := rec.Record(recordWork("gt-a", "h1")); err != nil {
		t.Fatal(err)
	}

	got := readRecords(t, path)[0]
	if len(got.Subjects) != submissionSubjectLimit {
		t.Fatalf("subjects = %q, want %d of them", got.Subjects, submissionSubjectLimit)
	}
	if got.Subjects[0] != "commit 0" {
		t.Errorf("first subject = %q, want the newest commit", got.Subjects[0])
	}
	if n := strings.Count(got.Numstat, "\n") + 1; n != submissionNumstatLimit {
		t.Errorf("numstat holds %d lines, want %d", n, submissionNumstatLimit)
	}
}

// A clone that cannot read the commits writes no subjects and says so through
// Warn, so the daemon log shows why the battery will read its own clone.
func TestSubmissionRecorderWarnsWhenTheCloneCannotReadTheCommits(t *testing.T) {
	t.Parallel()
	g := &fakeSubmissionGit{err: errors.New("bad revision 'h1'")}
	rec, path := newRecorder(t, g)
	var warned []string
	rec.Warn = func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }

	if _, err := rec.Record(recordWork("gt-a", "h1")); err != nil {
		t.Fatal(err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "gt-a") || !strings.Contains(warned[0], "bad revision") {
		t.Errorf("warnings = %q, want one naming the bead and the read error", warned)
	}
	if recs := readRecords(t, path); len(recs) != 1 || len(recs[0].Subjects) != 0 {
		t.Errorf("records = %+v, want one record with no subjects", recs)
	}
}

// A stat that cannot be read drops the subjects with it: a record holds
// subjects with their stat or neither, so the battery never asks about commits
// against a diff it does not have.
func TestSubmissionRecorderDropsSubjectsWhenTheStatCannotBeRead(t *testing.T) {
	t.Parallel()
	g := &fakeSubmissionGit{
		messages: map[string][]git.CommitMessage{"h1": {{Message: "fix: a"}}},
		numErr:   errors.New("bad object"),
	}
	rec, path := newRecorder(t, g)
	var warned []string
	rec.Warn = func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }

	if wrote, err := rec.Record(recordWork("gt-a", "h1")); err != nil || !wrote {
		t.Fatalf("Record = %v, %v; want a record written", wrote, err)
	}
	recs := readRecords(t, path)
	if len(recs) != 1 || len(recs[0].Subjects) != 0 || recs[0].Numstat != "" {
		t.Errorf("records = %+v, want no subjects and no stat", recs)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "stat") {
		t.Errorf("warnings = %q, want one naming the stat", warned)
	}
}

// The commits are read against the landing remote's target, the ref the
// landing merges onto, so a rig whose landing remote is not origin reads its
// own target.
func TestSubmissionRecorderReadsTheLandingRemoteTarget(t *testing.T) {
	t.Parallel()
	g := &fakeSubmissionGit{messages: map[string][]git.CommitMessage{"h1": {{Message: "fix: a"}}}}
	rec, _ := newRecorder(t, g)
	rec.Remote = "upstream"

	if _, err := rec.Record(recordWork("gt-a", "h1")); err != nil {
		t.Fatal(err)
	}
	want := []string{"messages upstream/main..h1", "numstat upstream/main..h1"}
	if strings.Join(g.logged, " | ") != strings.Join(want, " | ") {
		t.Errorf("git reads = %v, want %v", g.logged, want)
	}
}

// A write that fails is reported, and the submission is not remembered, so the
// next pass tries it again.
func TestSubmissionRecorderRetriesAFailedWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, SubmissionsSeenFile)
	g := &fakeSubmissionGit{messages: map[string][]git.CommitMessage{"h1": {{Message: "fix: a"}}}}
	rec := &SubmissionRecorder{Path: path, Git: g}

	if _, err := rec.Record(recordWork("gt-a", "h1")); err == nil {
		t.Fatal("Record into an unwritable path returned no error")
	}
	if _, err := rec.Record(recordWork("gt-a", "h1")); err == nil {
		t.Fatal("the retry after a failed write reported no error")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if wrote, err := rec.Record(recordWork("gt-a", "h1")); err != nil || !wrote {
		t.Fatalf("after the path clears = %v, %v; want the record written", wrote, err)
	}
}

func TestSubmissionsSeenPathIsUnderTheJevRuntime(t *testing.T) {
	t.Parallel()
	got := SubmissionsSeenPath("/town")
	want := filepath.Join("/town", ".runtime", "jev", "submissions-seen.jsonl")
	if got != want {
		t.Errorf("SubmissionsSeenPath = %q, want %q", got, want)
	}
}

// TestFirstSeenInAnswersTheOldestRecordForTheKey: the first sight is the first
// record of a bead at a head, so a later write of the same key does not move
// it, and a key with no record answers zero rather than a time (gt-8yk78).
func TestFirstSeenInAnswersTheOldestRecordForTheKey(t *testing.T) {
	t.Parallel()
	path := SubmissionsSeenPath(t.TempDir())
	later := time.Date(2026, 10, 25, 12, 10, 0, 0, time.UTC)
	first := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, rec := range []SubmissionRecord{
		{SeenAt: later, Bead: "gt-b", Head: "h1"},
		{SeenAt: later, Bead: "gt-a", Head: "h1"},
		{SeenAt: first, Bead: "gt-a", Head: "h1"},
		{SeenAt: later, Bead: "gt-a", Head: "h2"},
	} {
		if err := battery.Append(path, rec); err != nil {
			t.Fatalf("seeding the first-sight log: %v", err)
		}
	}
	lookup := FirstSeenIn(path)

	got, err := lookup("gt-a", "h1")
	if err != nil {
		t.Fatalf("FirstSeenIn: %v", err)
	}
	if !got.Equal(first) {
		t.Errorf("first sight = %s, want the oldest record's %s", got, first)
	}
	got, err = lookup("gt-a", "h3")
	if err != nil {
		t.Fatalf("FirstSeenIn for an unseen head: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("first sight of an unseen head = %s, want zero", got)
	}
}

// TestFirstSeenInReportsALogItCannotRead: a log that exists but cannot be read
// is an error, not an empty log: the caller must not read "I saw nothing" from
// "I could not look" (gt-8yk78).
func TestFirstSeenInReportsALogItCannotRead(t *testing.T) {
	t.Parallel()
	path := SubmissionsSeenPath(t.TempDir())
	// A directory where the log belongs: every open of it fails.
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := FirstSeenIn(path)("gt-a", "h1"); err == nil {
		t.Fatal("FirstSeenIn on an unreadable log = nil, want an error")
	}
}
