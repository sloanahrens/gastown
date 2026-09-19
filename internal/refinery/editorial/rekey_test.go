package editorial

import (
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// rekeyFixture is a rig-shaped clone: a repo on main with a real (local,
// bare) origin and a fetched origin/main tracking ref, so the backfill's
// "did this land?" ancestry check and its notes push both run for real.
type rekeyFixture struct {
	t       *testing.T
	repoDir string
	origin  string
	g       *git.Git
}

func newRekeyFixture(t *testing.T) *rekeyFixture {
	t.Helper()
	repoDir := initTestRepo(t)
	f := &rekeyFixture{t: t, repoDir: repoDir, origin: t.TempDir(), g: git.NewGit(repoDir)}
	f.git(repoDir, "branch", "-M", "main")
	f.git(f.origin, "init", "--bare")
	if _, err := f.g.AddRemote("origin", f.origin); err != nil {
		t.Fatalf("add remote origin: %v", err)
	}
	f.publish()
	return f
}

// git runs git in dir and fails the test on error.
func (f *rekeyFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v (in %s): %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// publish pushes main and refreshes origin/main, the way the merge queue
// leaves a rig clone after landing work.
func (f *rekeyFixture) publish() string {
	f.t.Helper()
	if err := f.g.Push("origin", "main", false); err != nil {
		f.t.Fatalf("push main: %v", err)
	}
	if err := f.g.Fetch("origin"); err != nil {
		f.t.Fatalf("fetch origin: %v", err)
	}
	sha, err := f.g.Rev("origin/main")
	if err != nil {
		f.t.Fatalf("rev origin/main: %v", err)
	}
	return sha
}

func (f *rekeyFixture) checkout(ref string) {
	f.t.Helper()
	f.git(f.repoDir, "checkout", "-q", ref)
}

// landOnMain merges branch into main with --no-ff — the shape every merge
// queue landing has — and publishes the result.
func (f *rekeyFixture) landOnMain(branch, message string) string {
	f.t.Helper()
	f.git(f.repoDir, "merge", "--no-ff", "-m", message, branch)
	merged, err := f.g.Rev("HEAD")
	if err != nil {
		f.t.Fatalf("rev merge commit: %v", err)
	}
	f.publish()
	return strings.TrimSpace(merged)
}

// writeMRNote writes an approve note for mr on head, with the patch-id of
// base..head unless patchID overrides it (the mismatch case).
func writeMRNote(t *testing.T, g *git.Git, mr, base, head, patchID string) Note {
	t.Helper()
	if patchID == "" {
		var err error
		patchID, err = g.PatchID(base, head)
		if err != nil {
			t.Fatalf("PatchID %s..%s: %v", base, head, err)
		}
	}
	n := Note{
		OMVersion:     "1.4.0",
		RubricSHA256:  "deadbeef",
		Rig:           "gastown",
		MR:            mr,
		Worker:        "slate",
		BaseSHA:       base,
		HeadSHA:       head,
		PatchID:       patchID,
		Score:         0.62,
		Verdict:       "approve",
		FindingsCount: 4,
		Attempt:       1,
		ReviewedAt:    time.Date(2026, 9, 11, 5, 18, 48, 0, time.UTC),
	}
	if err := WriteNote(g, n); err != nil {
		t.Fatalf("WriteNote %s on %s: %v", mr, head, err)
	}
	return n
}

// landedPatchID recomputes the patch-id of a commit's own diff, the check
// the editorial-coverage doctor applies before it calls a commit covered.
func landedPatchID(t *testing.T, g *git.Git, commit string) string {
	t.Helper()
	parents, err := g.Parents(commit)
	if err != nil {
		t.Fatalf("Parents %s: %v", commit, err)
	}
	if len(parents) == 0 {
		t.Fatalf("commit %s has no parents", commit)
	}
	patchID, err := g.PatchID(parents[0], commit)
	if err != nil {
		t.Fatalf("PatchID %s..%s: %v", parents[0], commit, err)
	}
	return patchID
}

// noteOnOrigin reads a note straight out of the bare origin, proving the
// backfill published it rather than only writing it locally.
func noteOnOrigin(t *testing.T, f *rekeyFixture, commit string) string {
	t.Helper()
	return f.git(f.origin, "notes", "--ref", NotesRef, "show", commit)
}

// TestRekeyNote_CopiesNoteOntoLandedMergeCommit is the gt-qvxf shape: a
// non-fast-forward merge landed and the note stayed on the reviewed branch
// tip, so the landed merge commit has none.
func TestRekeyNote_CopiesNoteOntoLandedMergeCommit(t *testing.T) {
	f := newRekeyFixture(t)
	base, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	f.git(f.repoDir, "checkout", "-q", "-b", "polecat/slate/gt-aivi")
	head := commitFile(t, f.repoDir, "feature.txt", "hello\n", "add feature")
	want := writeMRNote(t, f.g, "gt-wisp-c304", base, head, "")

	f.checkout("main")
	merged := f.landOnMain("polecat/slate/gt-aivi", "Merge polecat/slate/gt-aivi into main")
	if merged == head {
		t.Fatal("expected a merge commit, not a fast-forward")
	}

	const reason = "non-ff merge landed before its note was copied"
	res, err := RekeyNote(f.g, RekeyRequest{MR: "gt-wisp-c304", Landed: merged, Target: "main", Reason: reason})
	if err != nil {
		t.Fatalf("RekeyNote: %v", err)
	}
	if res.SourceCommit != head {
		t.Fatalf("source commit = %s, want the reviewed head %s", res.SourceCommit, head)
	}
	if len(res.Written) != 1 || res.Written[0] != merged {
		t.Fatalf("written = %v, want [%s]", res.Written, merged)
	}
	if !res.Pushed {
		t.Fatal("expected the notes ref to be pushed")
	}

	got, err := ReadNote(f.g, merged)
	if err != nil {
		t.Fatalf("ReadNote on landed commit %s: %v", merged, err)
	}
	if got.HeadSHA != merged {
		t.Fatalf("head_sha = %s, want the landed commit %s", got.HeadSHA, merged)
	}
	if got.RekeyedFrom != head {
		t.Fatalf("rekeyed_from = %q, want %q", got.RekeyedFrom, head)
	}
	if !got.Backfill || !got.PatchIDVerified {
		t.Fatalf("expected backfill and patch_id_verified, got %+v", got)
	}
	if got.BackfilledBy != "Test User" {
		t.Fatalf("backfilled_by = %q, want the repo's git user.name", got.BackfilledBy)
	}
	if got.BackfilledAt == nil {
		t.Fatal("expected backfilled_at to be stamped")
	}
	if !strings.Contains(got.BackfillReason, reason) {
		t.Fatalf("backfill_reason %q does not record the operator's reason %q", got.BackfillReason, reason)
	}
	if !strings.Contains(got.BackfillReason, "verified equal") {
		t.Fatalf("backfill_reason %q does not record the patch-id verification", got.BackfillReason)
	}
	if got.PatchID != want.PatchID {
		t.Fatalf("re-keyed note patch-id = %s, want the reviewed %s", got.PatchID, want.PatchID)
	}
	if wantID := landedPatchID(t, f.g, merged); got.PatchID != wantID {
		t.Fatalf("note patch-id %s does not match the landed diff %s — the coverage check would still read it as uncovered", got.PatchID, wantID)
	}
	// The note also has to survive the round trip through origin, or the
	// coverage check (run from another clone) never sees it.
	if published := noteOnOrigin(t, f, merged); !strings.Contains(published, `"rekeyed_from":"`+head+`"`) {
		t.Fatalf("origin's note does not carry rekeyed_from %s: %s", head, published)
	}
}

// TestRekeyNote_PatchIDMismatchRefuses: an approve note for the MR that
// reviews something other than what landed proves nothing about main, so the
// backfill must refuse and leave the notes ref untouched.
func TestRekeyNote_PatchIDMismatchRefuses(t *testing.T) {
	f := newRekeyFixture(t)
	base, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	f.git(f.repoDir, "checkout", "-q", "-b", "polecat/slate/gt-aivi")
	head := commitFile(t, f.repoDir, "feature.txt", "hello\n", "add feature")
	writeMRNote(t, f.g, "gt-wisp-c304", base, head, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	f.checkout("main")
	merged := f.landOnMain("polecat/slate/gt-aivi", "Merge polecat/slate/gt-aivi into main")

	_, err = RekeyNote(f.g, RekeyRequest{MR: "gt-wisp-c304", Landed: merged, Target: "main", Reason: "backfill after a failed copy"})
	if err == nil {
		t.Fatal("expected a patch-id mismatch to refuse the backfill")
	}
	if !strings.Contains(err.Error(), "patch-id mismatch") {
		t.Fatalf("error does not name the mismatch: %v", err)
	}
	if _, err := ReadNote(f.g, merged); !errors.Is(err, git.ErrNoNote) {
		t.Fatalf("a refused backfill must not write a note; got note with err %v", err)
	}
}

// TestRekeyNote_SecondParentCopy covers the acceptance rule that the proof
// lands on the merge commit AND on the second parent it brought in — here
// from a note keyed to a rehearsal head that is not on any branch, the
// 097ab8a/245987b shape.
func TestRekeyNote_SecondParentCopy(t *testing.T) {
	f := newRekeyFixture(t)
	base, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	f.git(f.repoDir, "checkout", "-q", "-b", "polecat/slate/gt-aivi")
	polecatHead := commitFile(t, f.repoDir, "feature.txt", "hello\n", "add feature")
	f.checkout("main")
	merged := f.landOnMain("polecat/slate/gt-aivi", "Merge polecat/slate/gt-aivi into main")

	// The reviewed head the note is keyed to: a rehearsal commit made in a
	// throwaway worktree and discarded after review, carrying the same diff
	// (so the same patch-id) as the branch that actually landed.
	f.git(f.repoDir, "checkout", "-q", "-b", "rehearsal", base)
	rehearsal := commitFile(t, f.repoDir, "feature.txt", "hello\n", "rehearsal of the reviewed change")
	f.checkout("main")
	rehearsalPatchID, err := f.g.PatchID(base, rehearsal)
	if err != nil {
		t.Fatalf("PatchID rehearsal: %v", err)
	}
	if want := landedPatchID(t, f.g, merged); rehearsalPatchID != want {
		t.Fatalf("fixture is wrong: rehearsal patch-id %s, landed diff %s", rehearsalPatchID, want)
	}
	writeMRNote(t, f.g, "gt-wisp-qb0y", base, rehearsal, rehearsalPatchID)

	const reason = "second-parent copy did not run before the merge landed"
	res, err := RekeyNote(f.g, RekeyRequest{
		MR: "gt-wisp-qb0y", Landed: merged, Target: "main", SecondParent: true, Reason: reason,
	})
	if err != nil {
		t.Fatalf("RekeyNote: %v", err)
	}
	if res.SourceCommit != rehearsal {
		t.Fatalf("source commit = %s, want the rehearsal head %s", res.SourceCommit, rehearsal)
	}
	if len(res.Targets) != 2 || res.Targets[0] != merged || res.Targets[1] != polecatHead {
		t.Fatalf("targets = %v, want [%s %s]", res.Targets, merged, polecatHead)
	}
	if len(res.Written) != 2 {
		t.Fatalf("written = %v, want both targets stamped", res.Written)
	}

	for _, target := range []string{merged, polecatHead} {
		note, err := ReadNote(f.g, target)
		if err != nil {
			t.Fatalf("ReadNote on %s: %v", target, err)
		}
		if note.HeadSHA != target {
			t.Fatalf("note on %s has head_sha %s", target, note.HeadSHA)
		}
		if note.RekeyedFrom != rehearsal {
			t.Fatalf("note on %s has rekeyed_from %q, want %q", target, note.RekeyedFrom, rehearsal)
		}
		if want, got := landedPatchID(t, f.g, target), note.PatchID; want != got {
			t.Fatalf("note on %s has patch-id %s, its own diff is %s", target, got, want)
		}
		if published := noteOnOrigin(t, f, target); published == "" {
			t.Fatalf("note on %s was not published to origin", target)
		}
	}
}

// TestRekeyNote_SecondParentOnNonMergeRefuses: --second-parent asks for a
// merge's second parent; on a plain commit there is none.
func TestRekeyNote_SecondParentOnNonMergeRefuses(t *testing.T) {
	f := newRekeyFixture(t)
	landed := commitFile(t, f.repoDir, "feature.txt", "hello\n", "add feature")
	f.publish()

	_, err := RekeyNote(f.g, RekeyRequest{MR: "gt-wisp-c304", Landed: landed, Target: "main", SecondParent: true, Reason: "backfill"})
	if err == nil {
		t.Fatal("expected --second-parent on a non-merge commit to refuse")
	}
	if !strings.Contains(err.Error(), "--second-parent") {
		t.Fatalf("error does not explain the refusal: %v", err)
	}
}

// TestRekeyNote_PushFailureIsLoud: the copy is worthless to every other
// clone until refs/notes/om is pushed, so a push failure must surface as an
// error — never as a warning — while leaving the local note in place for a
// re-run.
func TestRekeyNote_PushFailureIsLoud(t *testing.T) {
	f := newRekeyFixture(t)
	base, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	f.git(f.repoDir, "checkout", "-q", "-b", "polecat/slate/gt-aivi")
	head := commitFile(t, f.repoDir, "feature.txt", "hello\n", "add feature")
	writeMRNote(t, f.g, "gt-wisp-c304", base, head, "")
	f.checkout("main")
	merged := f.landOnMain("polecat/slate/gt-aivi", "Merge polecat/slate/gt-aivi into main")

	// Break only the push: origin/main is already fetched, so the ancestry
	// check still passes and the failure is a real push failure.
	f.git(f.repoDir, "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "gone"))

	res, err := RekeyNote(f.g, RekeyRequest{MR: "gt-wisp-c304", Landed: merged, Target: "main", Reason: "backfill"})
	if err == nil {
		t.Fatal("expected a push failure to be reported as an error")
	}
	if !strings.Contains(err.Error(), "NOT published") {
		t.Fatalf("error does not say the note was not published: %v", err)
	}
	if res == nil {
		t.Fatal("expected a partial result so the caller can report what was written")
	}
	if res.Pushed {
		t.Fatal("result claims the notes ref was pushed")
	}
	if len(res.Written) != 1 {
		t.Fatalf("written = %v, want the landed commit", res.Written)
	}
	if _, err := ReadNote(f.g, merged); err != nil {
		t.Fatalf("the local note should survive a failed push: %v", err)
	}
}

// TestRekeyNote_NoNoteForMRRefuses: with no verdict of its own there is
// nothing to copy — the command must not invent one, even when some other
// MR's note covers the same diff.
func TestRekeyNote_NoNoteForMRRefuses(t *testing.T) {
	f := newRekeyFixture(t)
	base, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	f.git(f.repoDir, "checkout", "-q", "-b", "polecat/slate/gt-aivi")
	head := commitFile(t, f.repoDir, "feature.txt", "hello\n", "add feature")
	writeMRNote(t, f.g, "gt-wisp-other", base, head, "")
	f.checkout("main")
	merged := f.landOnMain("polecat/slate/gt-aivi", "Merge polecat/slate/gt-aivi into main")

	_, err = RekeyNote(f.g, RekeyRequest{MR: "gt-wisp-c304", Landed: merged, Target: "main", Reason: "backfill"})
	if err == nil {
		t.Fatal("expected a refusal when the MR has no note of its own")
	}
	if !strings.Contains(err.Error(), "no om note for MR gt-wisp-c304") {
		t.Fatalf("error does not name the missing verdict: %v", err)
	}
	// The operator usually has the landed sha, not the MR id: the refusal
	// should say which MR's note actually covers this diff.
	if !strings.Contains(err.Error(), "gt-wisp-other") {
		t.Fatalf("error does not point at the MR that covers this diff: %v", err)
	}
	if _, err := ReadNote(f.g, merged); !errors.Is(err, git.ErrNoNote) {
		t.Fatalf("a refused backfill must not write a note; got note with err %v", err)
	}
}

// TestRekeyNote_RefusesToReplaceAnotherMRsNote: a notes ref holds one note
// per commit, so stamping here would delete another MR's proof for the same
// diff. Refuse and leave it alone.
func TestRekeyNote_RefusesToReplaceAnotherMRsNote(t *testing.T) {
	f := newRekeyFixture(t)
	base, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	f.git(f.repoDir, "checkout", "-q", "-b", "polecat/slate/gt-aivi")
	head := commitFile(t, f.repoDir, "feature.txt", "hello\n", "add feature")
	writeMRNote(t, f.g, "gt-wisp-c304", base, head, "")
	f.checkout("main")
	merged := f.landOnMain("polecat/slate/gt-aivi", "Merge polecat/slate/gt-aivi into main")
	other := writeMRNote(t, f.g, "gt-wisp-other", base, merged, "")

	_, err = RekeyNote(f.g, RekeyRequest{MR: "gt-wisp-c304", Landed: merged, Target: "main", Reason: "backfill"})
	if err == nil {
		t.Fatal("expected a refusal rather than replacing another MR's proof")
	}
	if !strings.Contains(err.Error(), "already carries MR gt-wisp-other") {
		t.Fatalf("error does not name the note it would replace: %v", err)
	}
	got, err := ReadNote(f.g, merged)
	if err != nil {
		t.Fatalf("ReadNote on the landed commit: %v", err)
	}
	if got.MR != other.MR {
		t.Fatalf("another MR's note was replaced: %+v", got)
	}
}

// TestRekeyNote_UnlandedCommitRefuses: re-keying proof onto a commit that
// never reached the target would be an unaudited provenance claim.
func TestRekeyNote_UnlandedCommitRefuses(t *testing.T) {
	f := newRekeyFixture(t)
	base, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	unlanded := commitFile(t, f.repoDir, "feature.txt", "hello\n", "local only, never pushed")
	writeMRNote(t, f.g, "gt-wisp-c304", base, unlanded, "")

	_, err = RekeyNote(f.g, RekeyRequest{MR: "gt-wisp-c304", Landed: unlanded, Target: "main", Reason: "backfill"})
	if err == nil {
		t.Fatal("expected an unlanded commit to refuse")
	}
	if !strings.Contains(err.Error(), "not reachable from origin/main") {
		t.Fatalf("error does not explain the refusal: %v", err)
	}
	// The refused run must leave the source note exactly as the review wrote
	// it — no backfill stamp on an unlanded commit.
	note, err := ReadNote(f.g, unlanded)
	if err != nil {
		t.Fatalf("ReadNote on the reviewed head: %v", err)
	}
	if note.Backfill || note.RekeyedFrom != "" {
		t.Fatalf("refused backfill stamped the note: %+v", note)
	}
}

// TestRekeyNote_RerunRepublishes: a re-run must not restamp the note (the
// audit trail keeps the timestamp of the run that established it) but must
// still publish, so a first run whose push failed can be repaired.
func TestRekeyNote_RerunRepublishes(t *testing.T) {
	f := newRekeyFixture(t)
	base, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	f.git(f.repoDir, "checkout", "-q", "-b", "polecat/slate/gt-aivi")
	head := commitFile(t, f.repoDir, "feature.txt", "hello\n", "add feature")
	writeMRNote(t, f.g, "gt-wisp-c304", base, head, "")
	f.checkout("main")
	merged := f.landOnMain("polecat/slate/gt-aivi", "Merge polecat/slate/gt-aivi into main")

	req := RekeyRequest{MR: "gt-wisp-c304", Landed: merged, Target: "main", Reason: "backfill"}
	if _, err := RekeyNote(f.g, req); err != nil {
		t.Fatalf("first RekeyNote: %v", err)
	}
	first, err := f.g.NotesShow(NotesRef, merged)
	if err != nil {
		t.Fatalf("NotesShow after first run: %v", err)
	}

	res, err := RekeyNote(f.g, req)
	if err != nil {
		t.Fatalf("second RekeyNote: %v", err)
	}
	if len(res.Written) != 0 {
		t.Fatalf("re-run stamped %v, want nothing (already covered)", res.Written)
	}
	if !res.Pushed {
		t.Fatal("a re-run must still publish the notes ref")
	}
	second, err := f.g.NotesShow(NotesRef, merged)
	if err != nil {
		t.Fatalf("NotesShow after second run: %v", err)
	}
	if first != second {
		t.Fatalf("re-run rewrote the note:\nfirst  %s\nsecond %s", first, second)
	}
}

// TestRekeyNote_RequiresReason keeps the audit trail honest: a backfill has
// to say why the copy is legitimate.
func TestRekeyNote_RequiresReason(t *testing.T) {
	f := newRekeyFixture(t)
	landed, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	if _, err := RekeyNote(f.g, RekeyRequest{MR: "gt-wisp-c304", Landed: landed, Target: "main"}); err == nil {
		t.Fatal("expected a missing --reason to be refused")
	}
}

// TestNote_BackfillFieldsMatchTheHandWrittenSchema pins the JSON field names
// to the notes the mayor backfilled by hand (097ab8a, 245987b, dfc8c98), so a
// later reader — the coverage check, a human, the om tooling — sees the same
// schema whether the copy was hand-written or run by this command.
func TestNote_BackfillFieldsMatchTheHandWrittenSchema(t *testing.T) {
	const historical = `{"om_version":"dev","rig":"gastown","mr":"gt-wisp-c304",` +
		`"base_sha":"ef5f153","head_sha":"097ab8a","patch_id":"dee5be9b","score":0.62,` +
		`"verdict":"approve","rekeyed_from":"476420c3b4c77284e3df49bd53abeced57c72384",` +
		`"backfill":true,"backfill_reason":"non-ff merge landed before gt-qvxf; patch-id of git diff ef5f153 097ab8a verified equal",` +
		`"backfilled_by":"mayor","backfilled_at":"2026-09-11T07:20:56+00:00",` +
		`"requested_by":"overseer hq-wisp-52qut","patch_id_verified":true}`

	var n Note
	if err := json.Unmarshal([]byte(historical), &n); err != nil {
		t.Fatalf("unmarshal hand-written backfill note: %v", err)
	}
	if n.RekeyedFrom != "476420c3b4c77284e3df49bd53abeced57c72384" || !n.Backfill || !n.PatchIDVerified {
		t.Fatalf("backfill fields did not round-trip: %+v", n)
	}
	if n.RequestedBy != "overseer hq-wisp-52qut" || n.BackfilledBy != "mayor" {
		t.Fatalf("provenance fields did not round-trip: %+v", n)
	}
	if want := time.Date(2026, 9, 11, 7, 20, 56, 0, time.UTC); n.BackfilledAt == nil || !n.BackfilledAt.Equal(want) {
		t.Fatalf("backfilled_at = %v, want %s", n.BackfilledAt, want)
	}

	data, err := json.Marshal(Note{RekeyedFrom: "abc", Backfill: true, PatchIDVerified: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"rekeyed_from"`, `"backfill"`, `"patch_id_verified"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("marshalled note is missing %s: %s", key, data)
		}
	}
	// A review-written note must not grow backfill fields.
	data, err = json.Marshal(Note{MR: "gt-wisp-x", Verdict: "approve"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"rekeyed_from", "backfill", "patch_id_verified"} {
		if strings.Contains(string(data), key) {
			t.Fatalf("review-written note carries %s: %s", key, data)
		}
	}
}
