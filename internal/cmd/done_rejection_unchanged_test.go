package cmd

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// The scenarios below reproduce gt-0jzd5 against real repositories: a branch is
// pushed, rejected by the refinery, and resubmitted from a rework that changed
// nothing about the content. Every scenario has a control that changes the
// content by the smallest step that counts as addressing a finding, because the
// guard has to tell "same diff" from "same branch" — the branch is deliberately
// the same in most of them.
//
// The rejected tip enters as a map (the MR bead's commit_sha, which
// beads.ParseMRFields reads in production), never as the branch ref: gt done
// pushes the branch on every submit, so the ref is this attempt's own content
// by the time the guard runs.

// TestRejectedAttemptsFromNotes pins what the guard reads out of a bead's
// notes: the branch, MR id and one-line reason of each rejection.
func TestRejectedAttemptsFromNotes(t *testing.T) {
	cases := []struct {
		name  string
		notes string
		want  []rejectedAttempt
	}{
		{
			name:  "no notes",
			notes: "",
			want:  nil,
		},
		{
			name:  "notes without a rejection",
			notes: "Found the flake: a leaked env var.\nBranch: polecat/zircon/gt-other",
			want:  nil,
		},
		{
			name: "single rejection",
			notes: `MERGE REJECTION (attempt 1): lint - docs-lint finding unaddressed
Branch: polecat/topaz/gt-glfh+mudjlvex
Target: main
MR: gt-wisp-v8j9`,
			want: []rejectedAttempt{{
				branch:  "polecat/topaz/gt-glfh+mudjlvex",
				mrID:    "gt-wisp-v8j9",
				summary: "(attempt 1): lint - docs-lint finding unaddressed",
			}},
		},
		{
			name: "each rejection contributes its own attempt",
			notes: `MERGE REJECTION (attempt 1): build - go build failed
Branch: polecat/a/gt-one+s1
Target: main
MR: gt-wisp-1
MERGE REJECTION (attempt 2): tests - gate red
Branch: polecat/b/gt-one+s2
Target: main
MR: gt-wisp-2`,
			want: []rejectedAttempt{
				{branch: "polecat/a/gt-one+s1", mrID: "gt-wisp-1", summary: "(attempt 1): build - go build failed"},
				{branch: "polecat/b/gt-one+s2", mrID: "gt-wisp-2", summary: "(attempt 2): tests - gate red"},
			},
		},
		{
			name: "the same MR recorded twice is one attempt",
			notes: `MERGE REJECTION (attempt 1): tests - gate red
Branch: polecat/zircon/gt-test
Target: main
MR: gt-wisp-1
MERGE REJECTION (attempt 1): tests - gate red
Branch: polecat/zircon/gt-test
Target: main
MR: gt-wisp-1`,
			want: []rejectedAttempt{{
				branch:  "polecat/zircon/gt-test",
				mrID:    "gt-wisp-1",
				summary: "(attempt 1): tests - gate red",
			}},
		},
		{
			name: "a later Branch: or MR: line in appended prose is not a rejection",
			notes: `MERGE REJECTION (attempt 1): tests - gate red
Branch: polecat/zircon/gt-test
Target: main
MR: gt-wisp-1
Cleanup: witness reclaimed the sandbox
Branch: polecat/zircon/gt-unrelated
MR: gt-wisp-999`,
			want: []rejectedAttempt{{
				branch:  "polecat/zircon/gt-test",
				mrID:    "gt-wisp-1",
				summary: "(attempt 1): tests - gate red",
			}},
		},
		{
			name: "refs/heads/ prefix and surrounding quotes are normalized away",
			notes: `MERGE REJECTION (attempt 1): tests - gate red
Branch: "refs/heads/polecat/zircon/gt-test"
Target: main
MR: "gt-wisp-1"`,
			want: []rejectedAttempt{{
				branch:  "polecat/zircon/gt-test",
				mrID:    "gt-wisp-1",
				summary: "(attempt 1): tests - gate red",
			}},
		},
		{
			name:  "a rejection naming no MR cannot be checked against",
			notes: "MERGE REJECTION (attempt 1): lint - unaddressed\nBranch: polecat/zircon/gt-test",
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rejectedAttemptsFromNotes(tc.notes)
			if len(got) != len(tc.want) {
				t.Fatalf("rejectedAttemptsFromNotes = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("rejectedAttemptsFromNotes[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// rejectedReworkFixture is a clone on a work branch that has already been
// pushed and rejected: rejectedSHA is the tip the rejected MR was submitted
// with, which is what its MR bead records in commit_sha.
type rejectedReworkFixture struct {
	seed        string // stands in for the shared origin/main branch
	polecat     string
	branch      string
	rejectedSHA string
}

// newRejectedReworkFixture builds origin.git with a base commit, clones it into
// a seed checkout (main) and a polecat checkout, and leaves the polecat on a
// pushed work branch whose only change is a line in shared.txt.
func newRejectedReworkFixture(t *testing.T) rejectedReworkFixture {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	polecat := filepath.Join(dir, "polecat")
	branch := "polecat/zircon/gt-test"

	runGitCmd(t, "", "init", "--bare", remote)
	// Point the bare repo's HEAD at main explicitly: git init's default branch
	// name is host-configurable, and the clone below checks out whatever HEAD
	// names.
	runGitCmd(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCmd(t, "", "clone", remote, seed)
	runGitCmd(t, seed, "config", "user.email", "seed@example.com")
	runGitCmd(t, seed, "config", "user.name", "Seed")
	writeTestFile(t, filepath.Join(seed, "shared.txt"), "base\n")
	writeTestFile(t, filepath.Join(seed, "keep.txt"), "keep\n")
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "base")
	runGitCmd(t, seed, "push", "origin", "main")

	runGitCmd(t, "", "clone", remote, polecat)
	runGitCmd(t, polecat, "config", "user.email", "polecat@example.com")
	runGitCmd(t, polecat, "config", "user.name", "Polecat")
	runGitCmd(t, polecat, "checkout", "-b", branch, "origin/main")

	writeTestFile(t, filepath.Join(polecat, "shared.txt"), "base\nwork\n")
	runGitCmd(t, polecat, "add", "-A")
	runGitCmd(t, polecat, "commit", "-m", "implement the feature")
	runGitCmd(t, polecat, "push", "origin", branch)

	return rejectedReworkFixture{
		seed:        seed,
		polecat:     polecat,
		branch:      branch,
		rejectedSHA: revParse(t, polecat, "HEAD"),
	}
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s in %s: %v", ref, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// rejectionNotes is the refinery's rejection record for one attempt: the format
// deadWorkerRecoveryRequest writes into the source bead's notes.
func rejectionNotes(branch, mrID string) string {
	return "MERGE REJECTION (attempt 1): lint - docs-lint finding unaddressed\n" +
		"Branch: " + branch + "\nTarget: main\nMR: " + mrID + "\n"
}

// tipsOf is the MR-bead side of the check in tests: the commit_sha each MR
// recorded when it was submitted.
func tipsOf(m map[string]string) func(string) (string, bool) {
	return func(mrID string) (string, bool) {
		sha, ok := m[mrID]
		return sha, ok
	}
}

// checkRefusal runs the guard the way gt done does: against the target branch,
// on the fixture's worktree.
func checkRefusal(t *testing.T, f rejectedReworkFixture, notes string, tips func(string) (string, bool)) error {
	t.Helper()
	return reportUnchangedSinceRejection(git.NewGit(f.polecat), notes, "gt-0jzd5", "origin/main", tips)
}

// TestReportUnchangedSinceRejection_RefusesZeroCommitRework is the reported
// bug: the polecat was slung the rejected branch, made no commits, and gt done
// submitted an MR whose patch-id equalled the rejected MR's.
func TestReportUnchangedSinceRejection_RefusesZeroCommitRework(t *testing.T) {
	f := newRejectedReworkFixture(t)

	err := checkRefusal(t, f, rejectionNotes(f.branch, "gt-wisp-v8j9"), tipsOf(map[string]string{"gt-wisp-v8j9": f.rejectedSHA}))
	if err == nil {
		t.Fatal("a rework that made no commit was accepted; the identical diff must be refused")
	}
	if !strings.Contains(err.Error(), "no change since the rejection: address the findings first") {
		t.Errorf("refusal does not name the reason: %v", err)
	}
	if !strings.Contains(err.Error(), "gt-wisp-v8j9") {
		t.Errorf("refusal does not name the rejected MR: %v", err)
	}
}

// TestReportUnchangedSinceRejection_RefusesContentIdenticalRebase covers the
// rework that looks like work: the polecat ran the formula's rebase step onto a
// newer target and nothing else. Patch-id is base-invariant, so the rebase does
// not launder the rejected diff.
func TestReportUnchangedSinceRejection_RefusesContentIdenticalRebase(t *testing.T) {
	f := newRejectedReworkFixture(t)

	writeTestFile(t, filepath.Join(f.seed, "keep.txt"), "keep\nmain touch\n")
	runGitCmd(t, f.seed, "add", "-A")
	runGitCmd(t, f.seed, "commit", "-m", "merged: other work")
	runGitCmd(t, f.seed, "push", "origin", "main")

	runGitCmd(t, f.polecat, "fetch", "origin")
	runGitCmd(t, f.polecat, "rebase", "origin/main")

	if err := checkRefusal(t, f, rejectionNotes(f.branch, "gt-wisp-v8j9"), tipsOf(map[string]string{"gt-wisp-v8j9": f.rejectedSHA})); err == nil {
		t.Fatal("a rebase that changed no content was accepted; the diff is still the rejected one")
	}
}

// TestReportUnchangedSinceRejection_RefusesIdenticalDiffOnANewBranch is the
// same no-op wearing a fresh branch name: the guard compares content, not
// identity, so a new branch carrying the rejected change-set is refused too.
func TestReportUnchangedSinceRejection_RefusesIdenticalDiffOnANewBranch(t *testing.T) {
	f := newRejectedReworkFixture(t)

	runGitCmd(t, f.polecat, "checkout", "-b", "polecat/zircon/gt-test+abc123", "origin/main")
	writeTestFile(t, filepath.Join(f.polecat, "shared.txt"), "base\nwork\n")
	runGitCmd(t, f.polecat, "add", "-A")
	runGitCmd(t, f.polecat, "commit", "-m", "implement the feature")

	if err := checkRefusal(t, f, rejectionNotes(f.branch, "gt-wisp-v8j9"), tipsOf(map[string]string{"gt-wisp-v8j9": f.rejectedSHA})); err == nil {
		t.Fatal("a new branch carrying the rejected diff was accepted; content is what was rejected")
	}
}

// TestReportUnchangedSinceRejection_AllowsFixCommit is the control that must
// keep working: one commit that answers the findings makes the diff different,
// so the same branch submits cleanly.
func TestReportUnchangedSinceRejection_AllowsFixCommit(t *testing.T) {
	f := newRejectedReworkFixture(t)

	writeTestFile(t, filepath.Join(f.polecat, "shared.txt"), "base\nwork\naddressed the finding\n")
	runGitCmd(t, f.polecat, "add", "-A")
	runGitCmd(t, f.polecat, "commit", "-m", "address the docs-lint finding")

	if err := checkRefusal(t, f, rejectionNotes(f.branch, "gt-wisp-v8j9"), tipsOf(map[string]string{"gt-wisp-v8j9": f.rejectedSHA})); err != nil {
		t.Fatalf("a rework that changed the content was refused: %v", err)
	}
}

// TestReportUnchangedSinceRejection_AllowsFixAlreadyPushedOverTheBranch is the
// false-positive shape the design has to survive: the rework pushed its fix, so
// origin/<branch> now holds the NEW content. Reading the branch ref instead of
// the MR's recorded tip would compare the attempt with itself and refuse a
// legitimate fix.
func TestReportUnchangedSinceRejection_AllowsFixAlreadyPushedOverTheBranch(t *testing.T) {
	f := newRejectedReworkFixture(t)

	writeTestFile(t, filepath.Join(f.polecat, "shared.txt"), "base\nwork\naddressed the finding\n")
	runGitCmd(t, f.polecat, "add", "-A")
	runGitCmd(t, f.polecat, "commit", "-m", "address the docs-lint finding")
	runGitCmd(t, f.polecat, "push", "origin", f.branch)

	if err := checkRefusal(t, f, rejectionNotes(f.branch, "gt-wisp-v8j9"), tipsOf(map[string]string{"gt-wisp-v8j9": f.rejectedSHA})); err != nil {
		t.Fatalf("a pushed fix was refused because the branch ref now holds it: %v", err)
	}
}

// TestReportUnchangedSinceRejection_UnrelatedRejectedMR checks that a stale
// rejection whose content this branch does not carry (a different branch's
// attempt, or the same branch before its fix) does not block submission.
func TestReportUnchangedSinceRejection_UnrelatedRejectedMR(t *testing.T) {
	f := newRejectedReworkFixture(t)

	other := "polecat/zircon/gt-other+s1"
	runGitCmd(t, f.polecat, "checkout", "-b", other, "origin/main")
	writeTestFile(t, filepath.Join(f.polecat, "keep.txt"), "keep\nother work\n")
	runGitCmd(t, f.polecat, "add", "-A")
	runGitCmd(t, f.polecat, "commit", "-m", "other work")
	otherSHA := revParse(t, f.polecat, "HEAD")
	runGitCmd(t, f.polecat, "checkout", f.branch)

	if err := checkRefusal(t, f, rejectionNotes(other, "gt-wisp-1"), tipsOf(map[string]string{"gt-wisp-1": otherSHA})); err != nil {
		t.Fatalf("a rejection naming other content blocked the submission: %v", err)
	}
}

// TestReportUnchangedSinceRejection_UnknownTip covers the MR records the guard
// cannot resolve — a purge-reaped MR bead, a missing commit_sha, a tip this
// clone never fetched. None is evidence that the content changed, but refusing
// on them strands a polecat with no way to clear the guard.
func TestReportUnchangedSinceRejection_UnknownTip(t *testing.T) {
	f := newRejectedReworkFixture(t)
	notes := rejectionNotes(f.branch, "gt-wisp-v8j9")

	cases := map[string]func(string) (string, bool){
		"no record of the MR":  tipsOf(nil),
		"a record with no sha": tipsOf(map[string]string{"gt-wisp-v8j9": ""}),
		"a tip that is not an object in this clone": tipsOf(map[string]string{
			"gt-wisp-v8j9": "0000000000000000000000000000000000000001",
		}),
	}
	for name, tips := range cases {
		t.Run(name, func(t *testing.T) {
			if err := checkRefusal(t, f, notes, tips); err != nil {
				t.Fatalf("an unresolvable rejected tip blocked the submission: %v", err)
			}
		})
	}
}

// TestReportUnchangedSinceRejection_NoRejectionNotes is the ordinary path: a
// bead that was never rejected is not gated on content at all.
func TestReportUnchangedSinceRejection_NoRejectionNotes(t *testing.T) {
	f := newRejectedReworkFixture(t)

	if err := checkRefusal(t, f, "Findings: look at the do_flush helper.\n", tipsOf(nil)); err != nil {
		t.Fatalf("notes without a rejection blocked the submission: %v", err)
	}
}
