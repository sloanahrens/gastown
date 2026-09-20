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
