package done

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
)

// These test gt done's revert report: what it refuses and says for a given
// detection. What git.DetectRevertedMerges finds in real repositories is
// pinned by TestIntegrationDetectRevertedMerges_* in
// done_revert_check_integration_test.go.

// fakeRevertGit answers the report's reads: subjects by commit, and a diff
// stat.
type fakeRevertGit struct {
	stat     string
	head     string
	subjects map[string]string
}

func (f fakeRevertGit) DiffStatThreeDot(base, head string) (string, error) { return f.stat, nil }

func (f fakeRevertGit) Rev(ref string) (string, error) {
	if f.head == "" {
		return "", errors.New("unknown revision")
	}
	return f.head, nil
}

func (f fakeRevertGit) CommitSubject(rev string) (string, error) {
	if s, ok := f.subjects[rev]; ok {
		return s, nil
	}
	return "", errors.New("unknown revision")
}

func detected(r git.RevertReport, err error) func() (git.RevertReport, error) {
	return func() (git.RevertReport, error) { return r, err }
}

// TestReportRevertedMergesRefusesWorkThatUndoesMergedCommits (gt-63sz): the
// refusal names each undone commit and path, says how to integrate, and never
// mentions the override flag, since agents read refusals and self-bypass.
func TestReportRevertedMergesRefusesWorkThatUndoesMergedCommits(t *testing.T) {
	t.Parallel()
	g := fakeRevertGit{stat: " keep.txt | 1 -\n", subjects: map[string]string{"aaaaaaaaaaaa": "merged: other work"}}
	report := git.RevertReport{Reverted: []git.RevertedMerge{{Commit: "aaaaaaaaaaaa", Paths: []string{"keep.txt", "new.txt"}}}}
	err := reportRevertedMergesWith(g, detected(report, nil), "origin/main")
	if err == nil {
		t.Fatal("a branch that reverts merged work was accepted")
	}
	for _, want := range []string{"refusing to submit", "aaaaaaaa merged: other work", "undoes: keep.txt", "undoes: new.txt", "git rebase origin/main", "git diff --stat origin/main...HEAD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "--allow-reverts") {
		t.Errorf("refusal advertises the override flag:\n%s", err)
	}
}

// TestReportRevertedMergesBoundsTheRefusal: a long list is cut at the report
// limits and says how many more there are; a subject git cannot read is
// named as unavailable rather than failing the refusal.
func TestReportRevertedMergesBoundsTheRefusal(t *testing.T) {
	t.Parallel()
	var report git.RevertReport
	for i := 0; i < revertReportLimit+3; i++ {
		report.Reverted = append(report.Reverted, git.RevertedMerge{
			Commit: fmt.Sprintf("c%011d", i),
			Paths:  []string{"a", "b", "c", "d", "e", "f"},
		})
	}
	err := reportRevertedMergesWith(fakeRevertGit{}, detected(report, nil), "origin/main")
	if err == nil {
		t.Fatal("refusal expected")
	}
	for _, want := range []string{"... and 3 more", "... and 2 more paths", "(subject unavailable)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), fmt.Sprintf("c%011d", revertReportLimit)) {
		t.Errorf("refusal lists commits past the limit:\n%s", err)
	}
}

// TestReportRevertedMergesFailsClosedWhenDetectionCannotRun: a check that
// could not run must not read as a check that passed.
func TestReportRevertedMergesFailsClosedWhenDetectionCannotRun(t *testing.T) {
	t.Parallel()
	err := reportRevertedMergesWith(fakeRevertGit{}, detected(git.RevertReport{}, errors.New("unknown revision origin/main")), "origin/main")
	if err == nil || !strings.Contains(err.Error(), "cannot verify branch against origin/main") {
		t.Fatalf("report = %v, want a refusal naming the target", err)
	}
}

// TestReportRevertedMergesPassesCleanAndRelocatedWork: a branch that undoes
// nothing submits, and one that only moves merged code within its package
// submits too, with the move noted (gt-x748o).
func TestReportRevertedMergesPassesCleanAndRelocatedWork(t *testing.T) {
	t.Parallel()
	g := fakeRevertGit{subjects: map[string]string{"bbbbbbbbbbbb": "add the summary line"}}
	if err := reportRevertedMergesWith(g, detected(git.RevertReport{}, nil), "origin/main"); err != nil {
		t.Fatalf("a clean branch was refused: %v", err)
	}
	relocated := []git.RevertedMerge{{Commit: "bbbbbbbbbbbb", Paths: []string{"internal/pkg/foo.go"}}}
	if err := reportRevertedMergesWith(g, detected(git.RevertReport{Relocated: relocated}, nil), "origin/main"); err != nil {
		t.Fatalf("a relocated block was refused: %v", err)
	}
	note := relocatedMergesNote(g, relocated)
	for _, want := range []string{"Relocated", "bbbbbbbb add the summary line", "moved: internal/pkg/foo.go"} {
		if !strings.Contains(note, want) {
			t.Errorf("relocation note lacks %q:\n%s", want, note)
		}
	}
	if relocatedMergesNote(g, nil) != "" {
		t.Error("a note with nothing relocated")
	}
}

// TestRevertCheckRecordsRefusal (gt-vsct7.6): a revert refusal appends one
// line to the town's attention ledger naming the bead, the worker and the
// branch, so the refusal reaches the overseer instead of dying with the pane.
func TestRevertCheckRecordsRefusal(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	reverted := []git.RevertedMerge{
		{Commit: "aaaaaaaaaaaa", Paths: []string{"keep.txt"}},
		{Commit: "bbbbbbbbbbbb", Paths: []string{"also.txt"}},
	}
	g := fakeRevertGit{head: "abcdef1234567890", subjects: map[string]string{"aaaaaaaaaaaa": "merged: other work"}}
	r := &doneRun{
		townRoot: town, rigName: "gastown", polecatName: "emerald",
		branch: "polecat/emerald/gt-x", issueID: "gt-x",
	}
	var warn strings.Builder
	err := reportRevertedMergesRecordingTo(&warn, r, g, detected(git.RevertReport{Reverted: reverted}, nil), "origin/main")
	if err == nil {
		t.Fatal("a branch that reverts merged work was accepted")
	}
	if warn.String() != "" {
		t.Errorf("warning = %q, want none on a successful record", warn.String())
	}

	refusals, rerr := attention.ReadRefusals(town)
	if rerr != nil {
		t.Fatalf("ReadRefusals: %v", rerr)
	}
	if len(refusals) != 1 {
		t.Fatalf("refusals = %+v, want exactly one line", refusals)
	}
	got := refusals[0]
	if got.Bead != "gt-x" || got.Rig != "gastown" || got.Worker != "emerald" ||
		got.Branch != "polecat/emerald/gt-x" || got.Head != "abcdef1234567890" ||
		got.Kind != attention.KindRevertGuard {
		t.Errorf("refusal = %+v, want the refused bead, worker and branch", got)
	}
	for _, want := range []string{"2 merged commit(s)", "aaaaaaaa merged: other work"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary %q lacks %q", got.Summary, want)
		}
	}
}

// TestRevertCheckRecordsNothingElse: a clean branch, a refusal that could not
// run the check, and a relocated-only report each leave the ledger empty. The
// throwaway-path and unchanged-since-rejection refusals are not this check's
// to record.
func TestRevertCheckRecordsNothingElse(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	r := &doneRun{townRoot: town, rigName: "gastown", polecatName: "emerald", branch: "b", issueID: "gt-x"}
	g := fakeRevertGit{head: "abcdef1234567890"}
	cases := []struct {
		name   string
		report git.RevertReport
		err    error
	}{
		{"clean", git.RevertReport{}, nil},
		{"relocated only", git.RevertReport{Relocated: []git.RevertedMerge{{Commit: "c1", Paths: []string{"a"}}}}, nil},
		{"detection failed", git.RevertReport{}, errors.New("unknown revision origin/main")},
	}
	for _, tc := range cases {
		var warn strings.Builder
		err := reportRevertedMergesRecordingTo(&warn, r, g, detected(tc.report, tc.err), "origin/main")
		if tc.err == nil && err != nil {
			t.Errorf("%s: refused a branch it should accept: %v", tc.name, err)
		}
	}
	refusals, err := attention.ReadRefusals(town)
	if err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 0 {
		t.Errorf("refusals = %+v, want none", refusals)
	}
}

// TestRevertCheckRecordFailureWarnsAndKeepsTheRefusal (gt-vsct7.6): a ledger
// write that fails prints one warning and leaves the refusal exactly as it
// was, so recording can never change gt done's exit code.
func TestRevertCheckRecordFailureWarnsAndKeepsTheRefusal(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A regular file where the runtime directory must go makes the write fail.
	if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := fakeRevertGit{head: "abcdef1234567890", subjects: map[string]string{"aaaaaaaaaaaa": "merged: other work"}}
	r := &doneRun{townRoot: town, rigName: "gastown", polecatName: "emerald", branch: "b", issueID: "gt-x"}
	report := git.RevertReport{Reverted: []git.RevertedMerge{{Commit: "aaaaaaaaaaaa", Paths: []string{"keep.txt"}}}}

	var warn strings.Builder
	err := reportRevertedMergesRecordingTo(&warn, r, g, detected(report, nil), "origin/main")
	if err == nil || !strings.Contains(err.Error(), "refusing to submit") {
		t.Fatalf("refusal = %v, want the revert refusal unchanged", err)
	}
	if n := strings.Count(warn.String(), "could not record the refusal"); n != 1 {
		t.Errorf("warnings = %q, want exactly one record warning", warn.String())
	}
}

// fakeNotesClient records the notes gt done appends to the source bead.
type fakeNotesClient struct {
	beads.Client
	id    string
	notes []string
	err   error
}

func (f *fakeNotesClient) AppendNotes(id, note string) error {
	f.id = id
	f.notes = append(f.notes, note)
	return f.err
}

// waivedRun is a doneRun whose source bead carries labels and a description.
func waivedRun(town string, issue *beads.Issue, bd beads.Client) *doneRun {
	return &doneRun{
		townRoot: town, rigName: "gastown", polecatName: "emerald", branch: "b", issueID: issue.ID,
		sourceIssue: issue, sourceBD: bd,
	}
}

// TestRevertGuardWaivesLabeledDeletionNamedByTheBead (gt-b8f9z): a bead labeled
// deletes-by-spec whose description names every reverted path is deleting what
// it says it deletes, so gt done submits and appends one notes line recording
// the waiver. A path undone by two commits counts once.
func TestRevertGuardWaivesLabeledDeletionNamedByTheBead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	bd := &fakeNotesClient{}
	issue := &beads.Issue{
		ID:          "gt-x",
		Labels:      []string{"deletes-by-spec"},
		Description: "Delete internal/old/a.go and internal/old/b.go; both are superseded.",
	}
	r := waivedRun(town, issue, bd)
	g := fakeRevertGit{head: "abcdef1234567890", subjects: map[string]string{"aaaaaaaaaaaa": "old work"}}
	reverted := []git.RevertedMerge{
		{Commit: "aaaaaaaaaaaa", Paths: []string{"internal/old/a.go", "internal/old/b.go"}},
		{Commit: "bbbbbbbbbbbb", Paths: []string{"internal/old/a.go"}},
	}

	var warn strings.Builder
	err := reportRevertedMergesRecordingTo(&warn, r, g, detected(git.RevertReport{Reverted: reverted}, nil), "origin/main")
	if err != nil {
		t.Fatalf("a labeled deletion its description names was refused: %v", err)
	}
	if warn.String() != "" {
		t.Errorf("warning = %q, want none", warn.String())
	}
	want := "revert guard waived by deletes-by-spec for 2 path(s): internal/old/a.go, internal/old/b.go"
	if bd.id != "gt-x" || len(bd.notes) != 1 || bd.notes[0] != want {
		t.Errorf("notes = %v on %q, want one line %q", bd.notes, bd.id, want)
	}

	refusals, rerr := attention.ReadRefusals(town)
	if rerr != nil {
		t.Fatalf("ReadRefusals: %v", rerr)
	}
	if len(refusals) != 0 {
		t.Errorf("refusals = %+v, want none for a waived submission", refusals)
	}
}

// TestRevertGuardRefusesLabeledDeletionTheDescriptionMisses (gt-b8f9z): the
// label alone waives nothing. A reverted path the description never names is
// still a refusal, and the refusal names that path.
func TestRevertGuardRefusesLabeledDeletionTheDescriptionMisses(t *testing.T) {
	t.Parallel()
	bd := &fakeNotesClient{}
	issue := &beads.Issue{
		ID:          "gt-x",
		Labels:      []string{"deletes-by-spec"},
		Description: "Delete internal/old/a.go, which the reader replaced.",
	}
	r := waivedRun(t.TempDir(), issue, bd)
	g := fakeRevertGit{subjects: map[string]string{"aaaaaaaaaaaa": "old work"}}
	reverted := []git.RevertedMerge{{Commit: "aaaaaaaaaaaa", Paths: []string{"internal/old/a.go", "internal/old/b.go"}}}

	var warn strings.Builder
	err := reportRevertedMergesRecordingTo(&warn, r, g, detected(git.RevertReport{Reverted: reverted}, nil), "origin/main")
	if err == nil {
		t.Fatal("a labeled deletion the description does not name was accepted")
	}
	if !strings.Contains(err.Error(), "refusing to submit") || !strings.Contains(err.Error(), "unnamed: internal/old/b.go") {
		t.Errorf("refusal does not name the path the description misses:\n%s", err)
	}
	if strings.Contains(err.Error(), "unnamed: internal/old/a.go") {
		t.Errorf("refusal names a path the description carries:\n%s", err)
	}
	if len(bd.notes) != 0 {
		t.Errorf("notes = %v, want none on a refusal", bd.notes)
	}
}

// TestRevertGuardWithoutTheLabelIsUnchanged (gt-b8f9z): a description that
// names every reverted path waives nothing without the label, and the refusal
// does not mention the label.
func TestRevertGuardWithoutTheLabelIsUnchanged(t *testing.T) {
	t.Parallel()
	bd := &fakeNotesClient{}
	issue := &beads.Issue{ID: "gt-x", Description: "Delete internal/old/a.go and internal/old/b.go."}
	r := waivedRun(t.TempDir(), issue, bd)
	g := fakeRevertGit{subjects: map[string]string{"aaaaaaaaaaaa": "old work"}}
	reverted := []git.RevertedMerge{{Commit: "aaaaaaaaaaaa", Paths: []string{"internal/old/a.go", "internal/old/b.go"}}}

	var warn strings.Builder
	err := reportRevertedMergesRecordingTo(&warn, r, g, detected(git.RevertReport{Reverted: reverted}, nil), "origin/main")
	if err == nil || !strings.Contains(err.Error(), "refusing to submit") {
		t.Fatalf("refusal = %v, want the unlabeled refusal", err)
	}
	if strings.Contains(err.Error(), "deletes-by-spec") {
		t.Errorf("an unlabeled refusal mentions the label:\n%s", err)
	}
	if len(bd.notes) != 0 {
		t.Errorf("notes = %v, want none without the label", bd.notes)
	}
}

// TestRevertGuardRefusesLabeledDeletionWithNoDescription (gt-b8f9z): an empty
// description names no path, so every reverted path is refused and listed.
func TestRevertGuardRefusesLabeledDeletionWithNoDescription(t *testing.T) {
	t.Parallel()
	bd := &fakeNotesClient{}
	issue := &beads.Issue{ID: "gt-x", Labels: []string{"deletes-by-spec"}}
	r := waivedRun(t.TempDir(), issue, bd)
	g := fakeRevertGit{subjects: map[string]string{"aaaaaaaaaaaa": "old work"}}
	reverted := []git.RevertedMerge{{Commit: "aaaaaaaaaaaa", Paths: []string{"internal/old/a.go", "internal/old/b.go"}}}

	var warn strings.Builder
	err := reportRevertedMergesRecordingTo(&warn, r, g, detected(git.RevertReport{Reverted: reverted}, nil), "origin/main")
	if err == nil {
		t.Fatal("an empty description waived a deletion")
	}
	for _, want := range []string{"unnamed: internal/old/a.go", "unnamed: internal/old/b.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%s", want, err)
		}
	}
	if len(bd.notes) != 0 {
		t.Errorf("notes = %v, want none on a refusal", bd.notes)
	}
}

// TestRevertGuardWaiverNoteFailureKeepsTheSubmission (gt-b8f9z): the waiver
// note is best-effort. A write that fails warns and the submission stands;
// recording can never turn an accepted branch into a refused one.
func TestRevertGuardWaiverNoteFailureKeepsTheSubmission(t *testing.T) {
	t.Parallel()
	bd := &fakeNotesClient{err: errors.New("dolt is down")}
	issue := &beads.Issue{
		ID:          "gt-x",
		Labels:      []string{"deletes-by-spec"},
		Description: "Delete internal/old/a.go.",
	}
	r := waivedRun(t.TempDir(), issue, bd)
	reverted := []git.RevertedMerge{{Commit: "aaaaaaaaaaaa", Paths: []string{"internal/old/a.go"}}}

	var warn strings.Builder
	err := reportRevertedMergesRecordingTo(&warn, r, fakeRevertGit{}, detected(git.RevertReport{Reverted: reverted}, nil), "origin/main")
	if err != nil {
		t.Fatalf("a failed waiver note refused the branch: %v", err)
	}
	if n := strings.Count(warn.String(), "could not record the deletes-by-spec waiver"); n != 1 {
		t.Errorf("warnings = %q, want exactly one waiver warning", warn.String())
	}
}

// TestRevertedPathsNamesOnlyAtAPathBoundary (gt-j5q4i): a reverted path counts
// as named only when the description carries it as a whole path. A longer path
// ending in it does not name it, so README.md and go.mod stay unnamed while the
// path the description really writes — closed by a sentence period — is waived.
func TestRevertedPathsNamesOnlyAtAPathBoundary(t *testing.T) {
	t.Parallel()
	reverted := []git.RevertedMerge{{Commit: "aaaaaaaaaaaa", Paths: []string{
		"README.md", "go.mod", "internal/x.go", "docs/README.md",
	}}}
	all, unnamed := revertedPaths(reverted, "Delete docs/README.md and internal/go.mod, then internal/x.go.")

	wantAll := []string{"README.md", "go.mod", "internal/x.go", "docs/README.md"}
	if !reflect.DeepEqual(all, wantAll) {
		t.Errorf("all = %q, want %q", all, wantAll)
	}
	wantUnnamed := []string{"README.md", "go.mod"}
	if !reflect.DeepEqual(unnamed, wantUnnamed) {
		t.Errorf("unnamed = %q, want %q", unnamed, wantUnnamed)
	}
}

// TestNamedInDescriptionBoundaries (gt-j5q4i): the boundary rule, case by case.
// A path character on either side disqualifies the match, a sentence's trailing
// period or comma does not, and an occurrence inside a longer path does not
// hide a later occurrence that stands on its own.
func TestNamedInDescriptionBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		description string
		path        string
		want        bool
	}{
		{"longer directory path does not name it", "Delete docs/README.md.", "README.md", false},
		{"longer directory path does not name go.mod", "Delete internal/go.mod.", "go.mod", false},
		{"extension suffix does not name it", "Keep internal/x.go.bak.", "internal/x.go", false},
		{"prefixed path does not name it", "Keep xinternal/x.go.", "internal/x.go", false},
		{"backticks", "Delete `internal/x.go` now.", "internal/x.go", true},
		{"double quotes", `Delete "internal/x.go" now.`, "internal/x.go", true},
		{"parentheses", "Delete (internal/x.go) now.", "internal/x.go", true},
		{"after a space", "Delete internal/x.go now.", "internal/x.go", true},
		{"start of the text", "internal/x.go is gone.", "internal/x.go", true},
		{"start of a line", "Unrelated.\ninternal/x.go is gone.", "internal/x.go", true},
		{"before a sentence period", "Delete internal/x.go.", "internal/x.go", true},
		{"before a sentence comma", "Delete internal/x.go, which we replaced.", "internal/x.go", true},
		{"standalone later occurrence", "Delete docs/README.md; README.md is unrelated.", "README.md", true},
		{"empty path names nothing", "Delete internal/x.go.", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := namedInDescription(tc.description, tc.path); got != tc.want {
				t.Errorf("namedInDescription(%q, %q) = %v, want %v", tc.description, tc.path, got, tc.want)
			}
		})
	}
}

// TestRevertGuardRefusesAPathNamedOnlyInsideALongerOne (gt-j5q4i): the waiver
// stands down for the paths the description really writes and refuses the ones
// it only mentions inside a longer path, naming them.
func TestRevertGuardRefusesAPathNamedOnlyInsideALongerOne(t *testing.T) {
	t.Parallel()
	bd := &fakeNotesClient{}
	issue := &beads.Issue{
		ID:          "gt-x",
		Labels:      []string{"deletes-by-spec"},
		Description: "Delete docs/README.md and internal/x.go.bak.",
	}
	r := waivedRun(t.TempDir(), issue, bd)
	g := fakeRevertGit{subjects: map[string]string{"aaaaaaaaaaaa": "old work"}}
	reverted := []git.RevertedMerge{{Commit: "aaaaaaaaaaaa", Paths: []string{"README.md", "internal/x.go"}}}

	var warn strings.Builder
	err := reportRevertedMergesRecordingTo(&warn, r, g, detected(git.RevertReport{Reverted: reverted}, nil), "origin/main")
	if err == nil {
		t.Fatal("a path the description names only inside a longer one was waived")
	}
	for _, want := range []string{"unnamed: README.md", "unnamed: internal/x.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%s", want, err)
		}
	}
	if len(bd.notes) != 0 {
		t.Errorf("notes = %v, want none on a refusal", bd.notes)
	}
}
