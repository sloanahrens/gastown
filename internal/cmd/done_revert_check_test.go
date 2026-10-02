package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
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
