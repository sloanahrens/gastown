package done

import (
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// TestRejectedAttemptsFromNotes pins what the guard reads out of a bead's
// notes: the branch, MR id and one-line reason of each rejection.
func TestRejectedAttemptsFromNotes(t *testing.T) {
	t.Parallel()
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
				kind:    "lint",
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
				{branch: "polecat/a/gt-one+s1", mrID: "gt-wisp-1", summary: "(attempt 1): build - go build failed", kind: "build"},
				{branch: "polecat/b/gt-one+s2", mrID: "gt-wisp-2", summary: "(attempt 2): tests - gate red", kind: "tests"},
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
				kind:    "tests",
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
				kind:    "tests",
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
				kind:    "tests",
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

// fakeReworkGit is the guard's view of a clone as a table: HEAD's sha and the
// patch-id each tip carries against the target. A tip with no entry is one the
// clone does not hold, so MergeBase fails on it as real git does. Patch-id
// itself (base-invariant, whitespace-sensitive) is git's to get right; the
// integration test runs these scenarios against real repositories.
type fakeReworkGit struct {
	head     string
	patchIDs map[string]string // tip sha -> patch-id against the target
}

func (g fakeReworkGit) Rev(ref string) (string, error) {
	if ref != "HEAD" || g.head == "" {
		return "", fmt.Errorf("fatal: ambiguous argument %q", ref)
	}
	return g.head, nil
}

func (g fakeReworkGit) MergeBase(target, sha string) (string, error) {
	if _, ok := g.patchIDs[sha]; !ok {
		return "", fmt.Errorf("fatal: Not a valid commit name %s", sha)
	}
	return "base-of-" + sha, nil
}

func (g fakeReworkGit) PatchIDVerbatim(base, head string) (string, error) {
	return g.patchIDs[head], nil
}

const (
	rejectedBranch = "polecat/zircon/gt-test"
	rejectedTip    = "rejected-tip"
	rejectedDiff   = "patch-rejected"
)

// reworkWith is a clone whose HEAD carries diff, next to the rejected tip.
func reworkWith(diff string) fakeReworkGit {
	return fakeReworkGit{head: "rework-tip", patchIDs: map[string]string{
		rejectedTip:  rejectedDiff,
		"rework-tip": diff,
	}}
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

// The tip the rejected MR recorded, never the branch ref: gt done pushes the
// branch on every submit, so the ref is this attempt's own content by the time
// the guard runs.
var rejectedMR = tipsOf(map[string]string{"gt-wisp-v8j9": rejectedTip})

// TestReportUnchangedSinceRejection_RefusesTheRejectedDiff is gt-0jzd5: a
// rework whose diff is the rejected one is refused whatever produced it — no
// commit at all, a rebase onto a newer target, or the same change on a fresh
// branch.
func TestReportUnchangedSinceRejection_RefusesTheRejectedDiff(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]fakeReworkGit{
		"no commit since the rejection": {head: rejectedTip, patchIDs: map[string]string{rejectedTip: rejectedDiff}},
		"a rebase or a new branch":      reworkWith(rejectedDiff),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := reportUnchangedSinceRejection(g, rejectionNotes(rejectedBranch, "gt-wisp-v8j9"), "gt-0jzd5", "origin/main", rejectedMR)
			if err == nil {
				t.Fatal("the rejected diff was accepted")
			}
			if !strings.Contains(err.Error(), "no change since the rejection: address the findings first") {
				t.Errorf("refusal does not name the reason: %v", err)
			}
			if !strings.Contains(err.Error(), "gt-wisp-v8j9") {
				t.Errorf("refusal does not name the rejected MR: %v", err)
			}
		})
	}
}

// TestReportUnchangedSinceRejection_AllowsAChangedDiff is the control: any
// change to the content, a whitespace-only fix included (gt-2colr), submits.
func TestReportUnchangedSinceRejection_AllowsAChangedDiff(t *testing.T) {
	t.Parallel()
	if err := reportUnchangedSinceRejection(reworkWith("patch-fixed"), rejectionNotes(rejectedBranch, "gt-wisp-v8j9"), "gt-0jzd5", "origin/main", rejectedMR); err != nil {
		t.Fatalf("a rework that changed the content was refused: %v", err)
	}
}

// TestReportUnchangedSinceRejection_PassesWhatItCannotJudge covers every input
// that is no evidence of an unchanged diff: refusing on one strands a polecat
// with no way to clear the guard.
func TestReportUnchangedSinceRejection_PassesWhatItCannotJudge(t *testing.T) {
	t.Parallel()
	unchanged := reworkWith(rejectedDiff)
	unchanged.patchIDs["other-tip"] = "patch-other"
	cases := []struct {
		name  string
		g     fakeReworkGit
		notes string
		tips  func(string) (string, bool)
	}{
		{"notes without a rejection", unchanged, "Findings: look at the do_flush helper.\n", tipsOf(nil)},
		{"a rejection of other content", unchanged, rejectionNotes("polecat/zircon/gt-other+s1", "gt-wisp-1"), tipsOf(map[string]string{"gt-wisp-1": "other-tip"})},
		{"no record of the MR", unchanged, rejectionNotes(rejectedBranch, "gt-wisp-v8j9"), tipsOf(nil)},
		{"a record with no sha", unchanged, rejectionNotes(rejectedBranch, "gt-wisp-v8j9"), tipsOf(map[string]string{"gt-wisp-v8j9": ""})},
		{"a tip that is not an object in this clone", unchanged, rejectionNotes(rejectedBranch, "gt-wisp-v8j9"), tipsOf(map[string]string{"gt-wisp-v8j9": "0000000000000000000000000000000000000001"})},
		{"HEAD does not resolve", fakeReworkGit{patchIDs: unchanged.patchIDs}, rejectionNotes(rejectedBranch, "gt-wisp-v8j9"), rejectedMR},
		{"an unknown Land Head:", unchanged, landRejection("gate", "1111111111111111111111111111111111111111"), tipsOf(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := reportUnchangedSinceRejection(tc.g, tc.notes, "gt-0jzd5", "origin/main", tc.tips); err != nil {
				t.Fatalf("the guard refused input it cannot judge: %v", err)
			}
		})
	}
}

// landRejection is the note Land() writes on the work bead: the rejected tip on
// a Head: line and the work bead (not an MR wisp) on the MR: line.
func landRejection(kind, head string) string {
	return land.FormatRejectionNote(land.RejectionNote{
		Kind: kind, Reason: "rejected", Branch: rejectedBranch, Target: "main", MR: "gt-0jzd5", Head: head,
	})
}

// TestReportUnchangedSinceRejection_ReadsLandsHeadLine: the guard takes the tip
// from Head: directly, so an unchanged resubmission after a Land rejection is
// refused with no MR record to look up (ADR 0004).
func TestReportUnchangedSinceRejection_ReadsLandsHeadLine(t *testing.T) {
	t.Parallel()
	notes := landRejection("gate", rejectedTip)
	noMRs := tipsOf(map[string]string{})
	if err := reportUnchangedSinceRejection(reworkWith(rejectedDiff), notes, "gt-0jzd5", "origin/main", noMRs); err == nil {
		t.Fatal("an unchanged resubmission after a Land rejection was accepted")
	}
	if err := reportUnchangedSinceRejection(reworkWith("patch-fixed"), notes, "gt-0jzd5", "origin/main", noMRs); err != nil {
		t.Fatalf("a changed resubmission was refused: %v", err)
	}
}

// A rejection the diff did not cause is not a reason to refuse the same diff
// (gt-ol9r8): the change-set is identical and correct, and nothing in it can be
// edited to answer a push that did not land or a bead-state refusal. Only the
// classes that judge the content (gate, review, tests, ...) and an unclassified
// rejection keep the refusal.
func TestReportUnchangedSinceRejection_OnlyDiffCausedKindsRefuse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind   string
		refuse bool
	}{
		{"gate", true},
		{"review", true},
		{"tests", true},
		{"editorial", true},
		{"", true},
		{"some_future_kind", true},
		{"conflict", false},
		{"not_pushed", false},
		{"empty", false},
		{"policy", false},
	}
	for _, tc := range cases {
		t.Run("kind="+tc.kind, func(t *testing.T) {
			t.Parallel()
			err := reportUnchangedSinceRejection(reworkWith(rejectedDiff), landRejection(tc.kind, rejectedTip), "gt-0jzd5", "origin/main", tipsOf(nil))
			if tc.refuse && err == nil {
				t.Fatalf("an unchanged resubmission after a %q rejection was accepted", tc.kind)
			}
			if !tc.refuse && err != nil {
				t.Fatalf("an unchanged resubmission after a %q rejection, which the diff did not cause, was refused: %v", tc.kind, err)
			}
		})
	}
}

// Each attempt is judged on its own class: a diff-caused rejection of this
// exact diff still refuses when a later attempt was rejected for a reason the
// diff did not cause.
func TestReportUnchangedSinceRejection_EarlierDiffCausedRejectionStillRefuses(t *testing.T) {
	t.Parallel()
	notes := land.FormatRejectionNote(land.RejectionNote{
		Attempt: 1, Kind: "gate", Reason: "gate red", Branch: rejectedBranch, Target: "main",
		MR: "gt-0jzd5", Head: rejectedTip,
	}) + "\n" + land.FormatRejectionNote(land.RejectionNote{
		Attempt: 2, Kind: "not_pushed", Reason: "head not on origin", Branch: rejectedBranch, Target: "main",
		MR: "gt-0jzd5", Head: "1111111111111111111111111111111111111111",
	})
	if err := reportUnchangedSinceRejection(reworkWith(rejectedDiff), notes, "gt-0jzd5", "origin/main", tipsOf(nil)); err == nil {
		t.Fatal("a later non-diff rejection lifted the refusal for an earlier gate rejection of the same diff")
	}
}

func TestRejectionKindFromSummary(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"(attempt 1): gate - gate failed on the merged tree": "gate",
		"(attempt 2): Not_Pushed - head not on origin":       "not_pushed",
		"(attempt 1): lint - docs - lint":                    "lint",
		"(attempt 1): gate failed on the merged tree":        "",
		"(attempt 1): head is not reachable - tip abc":       "",
		"(attempt 1): ":     "",
		"no attempt header": "",
	}
	for summary, want := range cases {
		if got := rejectionKindFromSummary(summary); got != want {
			t.Errorf("rejectionKindFromSummary(%q) = %q, want %q", summary, got, want)
		}
	}
}
