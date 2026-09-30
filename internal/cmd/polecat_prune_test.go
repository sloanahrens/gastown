package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
)

// oldEnoughSuffix returns a generated-branch timestamp suffix old enough to
// clear minRemoteBranchPruneAge, so a test's branch is judged purely on
// preservation/liveness rather than incidentally tripping the age gate.
func oldEnoughSuffix() string {
	return strconv.FormatInt(time.Now().Add(-2*minRemoteBranchPruneAge).UnixMilli(), 36)
}

// freshSuffix returns a generated-branch timestamp suffix from "just now",
// well inside minRemoteBranchPruneAge.
func freshSuffix() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 36)
}

// notFoundLookup simulates no polecat identity existing under any name —
// the common "truly stale, identity long retired" case.
func notFoundLookup(string) (polecat.State, error) {
	return "", polecat.ErrPolecatNotFound
}

// workingLookup simulates a polecat that is actively working — live, in the
// sense that matters for remote pruning.
func workingLookup(string) (polecat.State, error) {
	return polecat.StateWorking, nil
}

// fakeOpenPRLookup is the open-PR guard stubRemotePolecatBranchOpenPR
// installs, for a test that hands it to pruneRemotePolecatBranches directly.
func fakeOpenPRLookup(openPRs ...string) (func(string, string) bool, *[]git.PullRequestRef) {
	var calls []git.PullRequestRef
	return func(branch, headSHA string) bool {
		calls = append(calls, git.PullRequestRef{Branch: branch, HeadSHA: headSHA})
		return slices.Contains(openPRs, branch)
	}, &calls
}

// TestReportRemotePolecatPruneSummary pins the command's --remote summary:
// a run that kept PR-protected branches and pruned nothing must not report
// "No stale remote polecat branches found" (gas-fk4).
func TestReportRemotePolecatPruneSummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		res     remotePolecatPruneResult
		dryRun  bool
		want    []string
		notWant []string
	}{
		{name: "nothing", want: []string{"No stale remote polecat branches found."}},
		{name: "kept for open PR", res: remotePolecatPruneResult{OpenPR: 1}, want: []string{"1 remote branch(es) left in place: open PR exists (gas-fk4)"}, notWant: []string{"No stale remote"}},
		{name: "pruned", res: remotePolecatPruneResult{Pruned: 2}, want: []string{"Pruned 2 remote branch(es)."}, notWant: []string{"No stale remote", "open PR"}},
		{name: "dry run with a kept branch", res: remotePolecatPruneResult{Pruned: 1, OpenPR: 1}, dryRun: true, want: []string{"Would prune 1 remote branch(es).", "left in place: open PR exists"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			reportRemotePolecatPrune(&out, tc.res, tc.dryRun)
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("summary %q lacks %q", out.String(), w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out.String(), w) {
					t.Errorf("summary %q says %q", out.String(), w)
				}
			}
		})
	}
}

// fakePruneRepo is origin's polecat branches in memory. preserved names the
// branches whose work is on the prune target; deleted records the deletes.
type fakePruneRepo struct {
	defaultBranch string
	baseRef       string
	refs          []git.RemoteRef
	preserved     map[string]bool
	statusErr     map[string]error
	listErr       error
	fetchErr      error
	deleteErr     error

	fetched []string
	targets []string
	deleted []string // "<branch>@<hash>"
}

func newFakePruneRepo(branches ...string) *fakePruneRepo {
	f := &fakePruneRepo{defaultBranch: "main", preserved: map[string]bool{}, statusErr: map[string]error{}}
	for i, b := range branches {
		f.refs = append(f.refs, git.RemoteRef{Name: "refs/heads/" + b, Hash: fmt.Sprintf("sha%d", i)})
	}
	return f
}

func (f *fakePruneRepo) RemoteDefaultBranch() string { return f.defaultBranch }

func (f *fakePruneRepo) CleanDefaultBranchBaseRef(remote, defaultBranch string) string {
	if f.baseRef != "" {
		return f.baseRef
	}
	return remote + "/" + defaultBranch
}

func (f *fakePruneRepo) FetchPrune(remote string) error {
	f.fetched = append(f.fetched, remote)
	return f.fetchErr
}

func (f *fakePruneRepo) ListPushRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error) {
	return f.refs, f.listErr
}

func (f *fakePruneRepo) PushRemoteRefTargetStatus(remote string, ref git.RemoteRef, target string) (git.BranchPreservationStatus, error) {
	f.targets = append(f.targets, target)
	branch := strings.TrimPrefix(ref.Name, "refs/heads/")
	if err := f.statusErr[branch]; err != nil {
		return git.BranchPreservationStatus{}, err
	}
	return git.BranchPreservationStatus{Preserved: f.preserved[branch]}, nil
}

func (f *fakePruneRepo) DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, branch+"@"+expectedHash)
	return nil
}

var _ remotePruneRepo = (*fakePruneRepo)(nil)

func stalePolecatBranch(name string) string {
	return polecat.FormatGeneratedBranchName(name, "gt-"+name, oldEnoughSuffix())
}

// TestPruneRemotePolecatBranchesDeletesOnlyWhatEveryGateClears walks one
// origin through every gate: a stale, retired, preserved branch is deleted
// at the tip it was listed at; a fresh one, an unpreserved one, one whose
// status could not be read and a non-branch ref are left alone.
func TestPruneRemotePolecatBranchesDeletesOnlyWhatEveryGateClears(t *testing.T) {
	t.Parallel()
	stale, unpreserved, unreadable := stalePolecatBranch("stalecat"), stalePolecatBranch("wipcat"), stalePolecatBranch("oddcat")
	fresh := polecat.FormatGeneratedBranchName("freshcat", "gt-fresh", freshSuffix())
	repo := newFakePruneRepo(stale, fresh, unpreserved, unreadable)
	repo.refs = append(repo.refs, git.RemoteRef{Name: "refs/tags/" + stale, Hash: "tagsha"})
	repo.preserved[stale], repo.preserved[fresh], repo.preserved[unreadable] = true, true, true
	repo.statusErr[unreadable] = errors.New("merge-base failed")
	hasOpenPR, _ := fakeOpenPRLookup()

	var out bytes.Buffer
	result, err := pruneRemotePolecatBranches(&out, notFoundLookup, hasOpenPR, repo, false)
	if err != nil {
		t.Fatalf("pruneRemotePolecatBranches: %v", err)
	}
	if result.Pruned != 1 || strings.Join(repo.deleted, ",") != stale+"@sha0" {
		t.Fatalf("result %+v deleted %v, want only %s at sha0", result, repo.deleted, stale)
	}
	if !strings.Contains(out.String(), "deleted remote "+stale) {
		t.Errorf("output %q does not report the delete", out.String())
	}
	for _, target := range repo.targets {
		if target != "origin/main" {
			t.Errorf("preservation judged against %s, want origin/main", target)
		}
	}
}

// TestPruneRemotePolecatBranchesNeverPrunesAFreshBranch: a branch generated
// moments ago with nothing ahead of main looks exactly like a merged stale
// one; the age gate is a hard backstop even when no identity owns it
// (gt-527j).
func TestPruneRemotePolecatBranchesNeverPrunesAFreshBranch(t *testing.T) {
	t.Parallel()
	fresh := polecat.FormatGeneratedBranchName("freshcat", "gt-fresh", freshSuffix())
	repo := newFakePruneRepo(fresh)
	repo.preserved[fresh] = true
	hasOpenPR, _ := fakeOpenPRLookup()
	result, err := pruneRemotePolecatBranches(io.Discard, notFoundLookup, hasOpenPR, repo, false)
	if err != nil || result.Pruned != 0 || len(repo.deleted) != 0 {
		t.Fatalf("result %+v err %v deleted %v, want nothing pruned", result, err, repo.deleted)
	}
}

// TestPruneRemotePolecatBranchesNeverPrunesALiveWorkingPolecat: old enough
// and preserved, but its polecat is working: liveness blocks the delete on
// its own, and so does a lookup that cannot answer.
func TestPruneRemotePolecatBranchesNeverPrunesALiveWorkingPolecat(t *testing.T) {
	t.Parallel()
	for name, lookup := range map[string]polecatStateLookup{
		"working":   workingLookup,
		"no lookup": nil,
		"lookup fails": func(string) (polecat.State, error) {
			return "", errors.New("beads unavailable")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			branch := stalePolecatBranch("livecat")
			repo := newFakePruneRepo(branch)
			repo.preserved[branch] = true
			hasOpenPR, _ := fakeOpenPRLookup()
			result, err := pruneRemotePolecatBranches(io.Discard, lookup, hasOpenPR, repo, false)
			if err != nil || result.Pruned != 0 || len(repo.deleted) != 0 {
				t.Fatalf("result %+v err %v deleted %v, want the live polecat's branch kept", result, err, repo.deleted)
			}
		})
	}
}

// TestPruneRemotePolecatBranchesDryRunDeletesNothing: a dry run lists what a
// real run would delete and deletes none of it.
func TestPruneRemotePolecatBranchesDryRunDeletesNothing(t *testing.T) {
	t.Parallel()
	branch := stalePolecatBranch("prunepatch")
	repo := newFakePruneRepo(branch)
	repo.preserved[branch] = true
	hasOpenPR, _ := fakeOpenPRLookup()
	var out bytes.Buffer
	result, err := pruneRemotePolecatBranches(&out, notFoundLookup, hasOpenPR, repo, true)
	if err != nil || result.Pruned != 1 {
		t.Fatalf("dry run = %+v, %v; want Pruned 1", result, err)
	}
	if len(repo.deleted) != 0 {
		t.Errorf("dry run deleted %v", repo.deleted)
	}
	if !strings.Contains(out.String(), "Would delete remote: ") || !strings.Contains(out.String(), branch) {
		t.Errorf("dry-run output %q does not list %s", out.String(), branch)
	}
}

// TestPruneRemotePolecatBranchesJudgesAForkAgainstUpstream: in a fork-backed
// rig the clean base is upstream/<default>. It is refreshed first and every
// branch is judged against it, never against the fork's own origin/main.
func TestPruneRemotePolecatBranchesJudgesAForkAgainstUpstream(t *testing.T) {
	t.Parallel()
	branch := stalePolecatBranch("forkcat")
	repo := newFakePruneRepo(branch)
	repo.baseRef = "upstream/main"
	hasOpenPR, _ := fakeOpenPRLookup()
	result, err := pruneRemotePolecatBranches(io.Discard, notFoundLookup, hasOpenPR, repo, true)
	if err != nil || result.Pruned != 0 {
		t.Fatalf("result %+v err %v, want the fork-only branch kept", result, err)
	}
	if strings.Join(repo.fetched, ",") != "upstream" || strings.Join(repo.targets, ",") != "upstream/main" {
		t.Errorf("fetched %v, judged against %v; want upstream refreshed and used", repo.fetched, repo.targets)
	}

	repo.fetchErr = errors.New("upstream unreachable")
	if _, err := pruneRemotePolecatBranches(io.Discard, notFoundLookup, hasOpenPR, repo, true); err == nil || !strings.Contains(err.Error(), "refreshing upstream") {
		t.Errorf("an unrefreshed upstream = %v, want the refresh error", err)
	}
}

// TestPruneRemotePolecatBranchesLeavesABranchWithAnOpenPR: a branch that
// clears every other gate is still kept while a pull request points at it,
// since deleting it closes the PR as unmerged (gas-fk4). The guard is asked
// about the branch at the tip a delete would use, and a dry run reports the
// same skip instead of promising a delete.
func TestPruneRemotePolecatBranchesLeavesABranchWithAnOpenPR(t *testing.T) {
	t.Parallel()
	for _, dryRun := range []bool{false, true} {
		branch := stalePolecatBranch("prcat")
		repo := newFakePruneRepo(branch)
		repo.preserved[branch] = true
		hasOpenPR, calls := fakeOpenPRLookup(branch)
		var out bytes.Buffer
		result, err := pruneRemotePolecatBranches(&out, notFoundLookup, hasOpenPR, repo, dryRun)
		if err != nil || result.Pruned != 0 || result.OpenPR != 1 || len(repo.deleted) != 0 {
			t.Fatalf("dryRun=%v: result %+v err %v deleted %v, want the PR branch kept", dryRun, result, err, repo.deleted)
		}
		if len(*calls) != 1 || (*calls)[0] != (git.PullRequestRef{Branch: branch, HeadSHA: "sha0"}) {
			t.Errorf("dryRun=%v: open-PR guard called with %+v, want %s at sha0", dryRun, *calls, branch)
		}
		if !strings.Contains(out.String(), "open PR exists (gas-fk4)") || strings.Contains(out.String(), "Would delete") {
			t.Errorf("dryRun=%v: output %q", dryRun, out.String())
		}
	}
}

// TestPruneRemotePolecatBranchesReportsAFailedDeleteWithoutCountingIt: a
// delete origin refused (the tip moved) is reported and not counted.
func TestPruneRemotePolecatBranchesReportsAFailedDeleteWithoutCountingIt(t *testing.T) {
	t.Parallel()
	branch := stalePolecatBranch("movedcat")
	repo := newFakePruneRepo(branch)
	repo.preserved[branch] = true
	repo.deleteErr = errors.New("stale info")
	hasOpenPR, _ := fakeOpenPRLookup()
	var out bytes.Buffer
	result, err := pruneRemotePolecatBranches(&out, notFoundLookup, hasOpenPR, repo, false)
	if err != nil || result.Pruned != 0 {
		t.Fatalf("result %+v err %v, want nothing counted", result, err)
	}
	if !strings.Contains(out.String(), "stale info") {
		t.Errorf("output %q does not report the refused delete", out.String())
	}

	repo.listErr = errors.New("ls-remote failed")
	if _, err := pruneRemotePolecatBranches(io.Discard, notFoundLookup, hasOpenPR, repo, false); err == nil || !strings.Contains(err.Error(), "listing remote refs") {
		t.Errorf("a failed listing = %v, want it returned", err)
	}
}
