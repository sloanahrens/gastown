package editorial

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// initTestRepo creates a minimal git repo with one commit, for note
// round-trip tests. Mirrors internal/git's own test helper since that one
// is unexported and this is a different package.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test User")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "initial commit")

	return dir
}

func TestWriteNoteReadNote_RoundTrip(t *testing.T) {
	dir := initTestRepo(t)
	g := git.NewGit(dir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	want := Note{
		OMVersion:     "1.4.0",
		RubricSHA256:  "deadbeef",
		Rig:           "gastown",
		MR:            "gt-wisp-x",
		Worker:        "marble",
		BaseSHA:       "base123",
		HeadSHA:       head,
		PatchID:       "patch123",
		Score:         0.72,
		Verdict:       "approve",
		FindingsCount: 3,
		Attempt:       2,
		ReviewedAt:    time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC),
	}
	want.PriorFindings.Resolved = []string{"abc123456789"}
	want.PriorFindings.Unresolved = []string{"def123456789"}

	if err := WriteNote(g, want); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}

	got, err := ReadNote(g, head)
	if err != nil {
		t.Fatalf("ReadNote: %v", err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("ReadNote round-trip mismatch:\ngot  %+v\nwant %+v", *got, want)
	}
}

func TestReadNote_NoNoteReturnsErrNoNote(t *testing.T) {
	dir := initTestRepo(t)
	g := git.NewGit(dir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	if _, err := ReadNote(g, head); err != git.ErrNoNote {
		t.Fatalf("ReadNote on unreviewed commit: got %v, want git.ErrNoNote", err)
	}
}

// TestNoteJSON_MatchesSpecFieldNames asserts the marshaled note uses the
// exact field names from the design spec's example, since the note is
// meant to be read by humans and other tools (git notes --ref om show).
func TestNoteJSON_MatchesSpecFieldNames(t *testing.T) {
	n := Note{
		OMVersion:     "1.4.0",
		RubricSHA256:  "abc",
		Rig:           "gastown",
		MR:            "gt-wisp-x",
		Worker:        "marble",
		BaseSHA:       "base",
		HeadSHA:       "head",
		PatchID:       "patch",
		Score:         0.72,
		Verdict:       "approve",
		FindingsCount: 3,
		Attempt:       1,
		ReviewedAt:    time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC),
	}
	dir := initTestRepo(t)
	g := git.NewGit(dir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	n.HeadSHA = head

	if err := WriteNote(g, n); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
	raw, err := g.NotesShow(NotesRef, head)
	if err != nil {
		t.Fatalf("NotesShow: %v", err)
	}
	for _, want := range []string{
		`"om_version":"1.4.0"`, `"rubric_sha256":"abc"`, `"rig":"gastown"`,
		`"mr":"gt-wisp-x"`, `"worker":"marble"`, `"base_sha":"base"`,
		`"head_sha":"` + head + `"`, `"patch_id":"patch"`, `"score":0.72`,
		`"verdict":"approve"`, `"findings_count":3`, `"prior_findings"`,
		`"attempt":1`, `"reviewed_at"`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("note JSON missing %q, got: %s", want, raw)
		}
	}
}

// The note lookup keys on the diff, never on the commit a note sits on or on
// the MR that asked for it (gt-qa2p). These tests annotate stand-in shas they
// never create: the lookup reads the notes ref only, and a note's annotated
// object need not exist to be readable — which is exactly why a verdict keyed
// to a rehearsal head survives that head's disappearance.
const (
	diffLookedFor  = "patch-id-of-the-diff-being-reviewed"
	diffOther      = "patch-id-of-some-other-diff"
	rubricDeployed = "rubric-sha-now-deployed"
	rubricOther    = "rubric-sha-since-retired"
)

// recordedNote builds a note as a review would have written it.
func recordedNote(patchID, rubric, verdict, omVersion string, reviewedAt time.Time) Note {
	return Note{
		OMVersion:    omVersion,
		RubricSHA256: rubric,
		Rig:          "gastown",
		MR:           "gt-mr-1",
		Worker:       "marble",
		BaseSHA:      "base",
		PatchID:      patchID,
		Score:        0.74,
		Verdict:      verdict,
		Attempt:      1,
		ReviewedAt:   reviewedAt,
	}
}

// writeNoteOn attaches n to sha, standing in for the review that wrote it.
func writeNoteOn(t *testing.T, g *git.Git, sha string, n Note) {
	t.Helper()
	n.HeadSHA = sha
	if err := WriteNote(g, n); err != nil {
		t.Fatalf("WriteNote on %s: %v", sha, err)
	}
}

// TestFindVerdictForDiff_IgnoresVerdictsThatDoNotApply pins that widening the
// lookup from one head to the whole notes ref did not widen what counts as a
// verdict for this diff: the same four conditions recordedVerdictApplies
// applies to the note on a known head still decide, so answering from a note
// about something else is not possible.
func TestFindVerdictForDiff_IgnoresVerdictsThatDoNotApply(t *testing.T) {
	for _, tc := range []struct {
		name string
		note Note
		min  string
	}{
		{"another diff", recordedNote(diffOther, rubricDeployed, "approve", "1.4.0", time.Unix(100, 0)), ""},
		{"another rubric", recordedNote(diffLookedFor, rubricOther, "approve", "1.4.0", time.Unix(100, 0)), ""},
		{"a verdict this pipeline would not have accepted", recordedNote(diffLookedFor, rubricDeployed, "abstain", "1.4.0", time.Unix(100, 0)), ""},
		{"below the om version floor", recordedNote(diffLookedFor, rubricDeployed, "approve", "0.9.0", time.Unix(100, 0)), "1.0.0"},
		{"below the floor by carrying no version at all", recordedNote(diffLookedFor, rubricDeployed, "approve", "", time.Unix(100, 0)), "1.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := git.NewGit(initTestRepo(t))
			writeNoteOn(t, g, "1111111111111111111111111111111111111111", tc.note)

			got, err := FindVerdictForDiff(g, diffLookedFor, rubricDeployed, tc.min)
			if err != nil {
				t.Fatalf("FindVerdictForDiff: %v", err)
			}
			if got != nil {
				t.Fatalf("FindVerdictForDiff = verdict %q for %s on %s, want none: the note does not answer for this diff",
					got.Note.Verdict, got.Note.PatchID, got.Commit)
			}
		})
	}
}

// TestFindVerdictForDiff_AbsentNotesRefIsNoVerdict pins that "nothing was ever
// reviewed here" and "nothing matching was found" are the same answer and not
// an error: an absent ref is a first review, which must proceed.
func TestFindVerdictForDiff_AbsentNotesRefIsNoVerdict(t *testing.T) {
	g := git.NewGit(initTestRepo(t))

	got, err := FindVerdictForDiff(g, diffLookedFor, rubricDeployed, "")
	if err != nil {
		t.Fatalf("FindVerdictForDiff on an absent notes ref: %v", err)
	}
	if got != nil {
		t.Fatalf("FindVerdictForDiff = %+v, want none", got)
	}
}

// TestFindVerdictForDiff_MostRecentlyReviewedMatchGoverns pins which verdict
// answers when several do, and that the MR id is deliberately not part of the
// key. The re-rolled verdict is the one the MR's push is authorized by, and a
// re-minted MR carrying the same diff is the same measurement: scoping the
// lookup to the MR would hand a caller the roll-until-it-clears bypass one
// bead id further along.
func TestFindVerdictForDiff_MostRecentlyReviewedMatchGoverns(t *testing.T) {
	g := git.NewGit(initTestRepo(t))
	first := recordedNote(diffLookedFor, rubricDeployed, "request_changes", "1.4.0", time.Unix(1000, 0).UTC())
	first.MR, first.Score = "gt-mr-1", 0.56
	rolled := recordedNote(diffLookedFor, rubricDeployed, "approve", "1.4.0", time.Unix(2000, 0).UTC())
	rolled.MR, rolled.Score = "gt-mr-9", 0.84

	writeNoteOn(t, g, "1111111111111111111111111111111111111111", first)
	writeNoteOn(t, g, "2222222222222222222222222222222222222222", rolled)

	got, err := FindVerdictForDiff(g, diffLookedFor, rubricDeployed, "")
	if err != nil {
		t.Fatalf("FindVerdictForDiff: %v", err)
	}
	if got == nil {
		t.Fatal("FindVerdictForDiff = none, want the re-rolled verdict")
	}
	if got.Commit != "2222222222222222222222222222222222222222" || got.Note.Score != 0.84 {
		t.Errorf("governed by %s (score %.2f), want the later verdict on 2222222 (score 0.84)",
			got.Commit, got.Note.Score)
	}
}

// TestFindVerdictForDiff_SkipsUnparseableNotes pins that one unreadable note
// does not make a diff's verdict unfindable: the ref is shared with every
// writer that ever touched it.
func TestFindVerdictForDiff_SkipsUnparseableNotes(t *testing.T) {
	g := git.NewGit(initTestRepo(t))
	if err := g.NotesAdd(NotesRef, "1111111111111111111111111111111111111111", "not json"); err != nil {
		t.Fatalf("NotesAdd: %v", err)
	}
	want := recordedNote(diffLookedFor, rubricDeployed, "approve", "1.4.0", time.Unix(1000, 0).UTC())
	writeNoteOn(t, g, "2222222222222222222222222222222222222222", want)

	got, err := FindVerdictForDiff(g, diffLookedFor, rubricDeployed, "")
	if err != nil {
		t.Fatalf("FindVerdictForDiff: %v", err)
	}
	if got == nil || got.Commit != "2222222222222222222222222222222222222222" {
		t.Fatalf("FindVerdictForDiff = %+v, want the parseable note on 2222222", got)
	}
}

// TestReviewedLaterThan_IsDeterministicOnATimestampTie pins the tie-break that
// keeps "which verdict governs" independent of the order the notes ref lists
// them in — two notes reviewed in the same instant must still order the same
// way every run.
func TestReviewedLaterThan_IsDeterministicOnATimestampTie(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	lo := Note{HeadSHA: "1111111111111111111111111111111111111111", ReviewedAt: at}
	hi := Note{HeadSHA: "9999999999999999999999999999999999999999", ReviewedAt: at}

	if !reviewedLaterThan(hi, lo) || reviewedLaterThan(lo, hi) {
		t.Errorf("same-instant verdicts did not order by reviewed head")
	}
	if reviewedLaterThan(hi, hi) {
		t.Errorf("a verdict is not later than itself")
	}

	later := hi
	later.ReviewedAt = at.Add(time.Second)
	if !reviewedLaterThan(later, hi) {
		t.Errorf("a later reviewed_at did not win over the head sha")
	}
}

func TestAttemptHistory_CarriesEveryPriorVerdictForward(t *testing.T) {
	first := NoteAttempt{Score: 0.74, Verdict: "approve", Attempt: 1, ReviewedAt: time.Unix(1000, 0).UTC()}
	second := NoteAttempt{Score: 0.50, Verdict: "request_changes", Attempt: 2, ReviewedAt: time.Unix(2000, 0).UTC()}
	third := NoteAttempt{Score: 0.81, Verdict: "approve", Attempt: 3, ReviewedAt: time.Unix(3000, 0).UTC()}

	legacy := &Note{Score: first.Score, Verdict: first.Verdict, Attempt: first.Attempt, ReviewedAt: first.ReviewedAt}
	recorded := &Note{
		Score: second.Score, Verdict: second.Verdict, Attempt: second.Attempt, ReviewedAt: second.ReviewedAt,
		Attempts: []NoteAttempt{first, second},
	}

	for _, tc := range []struct {
		name string
		prev *Note
		want []NoteAttempt
	}{
		{"no prior note", nil, []NoteAttempt{third}},
		// A note written before Attempts existed keeps its only verdict.
		{"prior note without history", legacy, []NoteAttempt{first, third}},
		// A note that already carries history is not re-summarised, so its
		// own top-level verdict is neither duplicated nor dropped.
		{"prior note with history", recorded, []NoteAttempt{first, second, third}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := attemptHistory(tc.prev, third)
			if len(got) != len(tc.want) {
				t.Fatalf("attemptHistory = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("attemptHistory[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
