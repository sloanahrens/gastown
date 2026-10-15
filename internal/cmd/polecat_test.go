package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
)

// notFoundBeads is a recoveryBeads whose every Show answers ErrNotFound and
// which has no MR for any branch: a rig where the source issue was purged.
type notFoundBeads struct{ beads.Client }

func (notFoundBeads) Show(string) (*beads.Issue, error) { return nil, beads.ErrNotFound }

func (notFoundBeads) FindMRForBranchAny(string) (*beads.Issue, error) {
	return nil, beads.ErrNotFound
}

// A purged source bead is absent, not unreadable: the active-MR and branch-MR
// lookups already tolerate ErrNotFound, and the source-issue lookup must too,
// or every sweep forces NEEDS_RECOVERY on a polecat whose bead was deleted.
func TestRecoveryTargetRefsSourceIssueNotFound(t *testing.T) {
	t.Parallel()
	_, lookupFailed, _, _ := recoveryTargetRefs(notFoundBeads{}, "gt-purged", "", "polecat/nitro")
	if lookupFailed {
		t.Fatal("MQLookupFailed = true for a source issue that is not found, want false")
	}
}

// A source-issue read that fails for any other reason still fails closed.
func TestRecoveryTargetRefsSourceIssueReadErrorFailsClosed(t *testing.T) {
	t.Parallel()
	bd := failingShowBeads{notFoundBeads{}}
	_, lookupFailed, _, _ := recoveryTargetRefs(bd, "gt-abc", "", "")
	if !lookupFailed {
		t.Fatal("MQLookupFailed = false for an unreadable source issue, want true")
	}
}

type failingShowBeads struct{ notFoundBeads }

func (failingShowBeads) Show(string) (*beads.Issue, error) { return nil, errors.New("dolt down") }

// The verdict flips to NEEDS_RECOVERY on a failed reconcile, so the flags
// DecideWorkstate derived from SAFE_TO_NUKE must flip with it: --json must not
// report safe_to_nuke true beside needs_recovery true. The rest of the status
// must read like a native NEEDS_RECOVERY for the same recorded cleanup_status —
// reason, reuse_status, counts_toward_capacity and recovery actions — or the
// seat reads as one that is neither at capacity nor recoverable (gt-y6mhx).
func TestReconcileCleanupStatusFailureClearsSafeToNuke(t *testing.T) {
	t.Parallel()
	for name, updater := range map[string]beads.Client{
		"update fails": func() beads.Client {
			u := newCleanupUpdater(t, polecat.CleanupUnpushed)
			u.err = errors.New("bd update failed")
			return u
		}(),
		"updater unavailable": nil,
	} {
		t.Run(name, func(t *testing.T) {
			status := &RecoveryStatus{
				CleanupStatus:        polecat.CleanupUnpushed,
				Verdict:              "SAFE_TO_NUKE",
				Reason:               "reusable",
				ReuseStatus:          "idle-preserved",
				Branch:               "polecat/nitro",
				MQStatus:             "submitted",
				SafeToNuke:           true,
				Reusable:             true,
				CountsTowardCapacity: true,
			}
			reconcileCleanupStatusIfSafe(status, updater, "gt-gastown-polecat-nitro", &polecat.Polecat{State: polecat.StateIdle}, &beads.AgentFields{
				AgentState:    string(beads.AgentStateIdle),
				CleanupStatus: string(polecat.CleanupUnpushed),
			})

			if status.Verdict != "NEEDS_RECOVERY" || !status.NeedsRecovery {
				t.Fatalf("verdict = %q needs_recovery = %v, want NEEDS_RECOVERY true", status.Verdict, status.NeedsRecovery)
			}
			if status.SafeToNuke || status.Reusable {
				t.Errorf("safe_to_nuke=%v reusable=%v, want both false beside NEEDS_RECOVERY",
					status.SafeToNuke, status.Reusable)
			}
			// DecideWorkstate's cleanup predicate counts toward capacity when it
			// blocks on a recorded status, and the seat never leaves recovery
			// while that status stands (workstate.go).
			if !status.CountsTowardCapacity {
				t.Error("counts_toward_capacity = false, want true — a recorded cleanup_status that blocks counts")
			}
			if status.Reason != "cleanup-"+string(polecat.CleanupUnpushed) {
				t.Errorf("reason = %q, want the native cleanup predicate's reason %q",
					status.Reason, "cleanup-"+string(polecat.CleanupUnpushed))
			}
			if status.ReuseStatus != "idle-recovery-needed" {
				t.Errorf("reuse_status = %q, want \"idle-recovery-needed\"", status.ReuseStatus)
			}
			if len(status.RecoveryActions) == 0 {
				t.Error("recovery_actions is empty beside NEEDS_RECOVERY, want the action that clears the failed write")
			}
			if len(status.Blockers) != 1 || !strings.HasPrefix(status.Blockers[0], cleanupReconcileFailedBlocker) {
				t.Errorf("blockers = %v, want the single %s blocker naming the failed write",
					status.Blockers, cleanupReconcileFailedBlocker)
			}
		})
	}
}

// failCleanupReconcile names an unknown or missing recorded status the way
// DecideWorkstate does, so both spellings of "no usable status" read alike
// (gt-y6mhx).
func TestFailCleanupReconcileNamesUnknownStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []polecat.CleanupStatus{"", polecat.CleanupUnknown} {
		got := &RecoveryStatus{CleanupStatus: status}
		failCleanupReconcile(got, cleanupReconcileFailedBlocker+": updater unavailable")
		if got.Reason != "cleanup-unknown" {
			t.Errorf("CleanupStatus %q: reason = %q, want cleanup-unknown", status, got.Reason)
		}
	}
}

// --force bypasses the safety refusal, so its dry-run must not count one, and
// the preview must name the session and worktree the nuke will act on: the
// rig's registered session prefix and the Manager's clone path.
func TestPolecatNukeForceDryRunSummary(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "gastown", Path: t.TempDir()}
	reg := session.NewPrefixRegistry()
	reg.Register("zz", "gastown")
	target := polecatTarget{rigName: "gastown", polecatName: "basalt", mgr: polecat.NewManager(r, nil, nil, reg), r: r}
	blocked := func(polecatTarget) *SafetyCheckResult { return &SafetyCheckResult{Blocked: true} }
	noList := func(polecatTarget, *SafetyCheckResult) {}
	noPreserveRisk := func(polecatTarget) nukePreviewRefusal { return nukePreviewRefusal{} }

	var out bytes.Buffer
	if previewNukeDryRunBatch(&out, []polecatTarget{target}, true, reg, nukePreviewDeps{
		Check: blocked, ListSafety: noList, PreserveRisk: noPreserveRisk,
	}) != 0 {
		t.Error("forced preview counted a refusal that --force bypasses")
	}
	if got := dryRunNukeSummary(1, 0); got != "Would nuke 1 polecat(s)." {
		t.Errorf("summary = %q, want the unrefused summary", got)
	}
	text := out.String()
	if strings.Contains(text, "refuse") {
		t.Errorf("forced preview mentions a refusal:\n%s", text)
	}
	for _, want := range []string{
		"Would nuke gastown/basalt:",
		"Kill session: zz-basalt\n",
		"Delete worktree: " + target.mgr.ClonePath("basalt") + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("preview lacks %q:\n%s", want, text)
		}
	}

	out.Reset()
	if previewNukeDryRunBatch(&out, []polecatTarget{target}, false, reg, nukePreviewDeps{
		Check: blocked, ListSafety: noList, PreserveRisk: noPreserveRisk,
	}) != 1 {
		t.Error("unforced preview of a blocked polecat did not count a refusal")
	}
	if !strings.Contains(out.String(), "Would refuse to nuke gastown/basalt without --force:") {
		t.Errorf("unforced preview does not say it would refuse:\n%s", out.String())
	}
}

// nukePreviewTarget is a target whose preview needs no live town: the rig's
// registry prefix and the Manager's clone path are the only facts it reads.
func nukePreviewTarget(t *testing.T, reg *session.PrefixRegistry, name string) polecatTarget {
	t.Helper()
	r := &rig.Rig{Name: "gastown", Path: t.TempDir()}
	return polecatTarget{rigName: "gastown", polecatName: name, mgr: polecat.NewManager(r, nil, nil, reg), r: r}
}

// nukeSafetyRefusal refuses the WHOLE batch when one target is blocked, so the
// dry-run of that batch must not promise to nuke the clean targets beside it:
// "Would nuke c" for a batch that nukes nothing is the preview disagreeing with
// the run it claims to describe (gt-y6mhx).
func TestPreviewNukeDryRunRefusesTheWholeBatch(t *testing.T) {
	t.Parallel()
	reg := session.NewPrefixRegistry()
	reg.Register("zz", "gastown")
	targets := []polecatTarget{
		nukePreviewTarget(t, reg, "basalt"),
		nukePreviewTarget(t, reg, "flint"),
		nukePreviewTarget(t, reg, "chert"),
	}
	check := func(p polecatTarget) *SafetyCheckResult {
		return &SafetyCheckResult{Polecat: "gastown/" + p.polecatName, Blocked: p.polecatName == "flint"}
	}

	var out bytes.Buffer
	refused := previewNukeDryRunBatch(&out, targets, false, reg, nukePreviewDeps{
		Check:        check,
		ListSafety:   func(polecatTarget, *SafetyCheckResult) {},
		PreserveRisk: func(polecatTarget) nukePreviewRefusal { return nukePreviewRefusal{} },
	})
	if refused != len(targets) {
		t.Errorf("refused = %d of %d, want every target refused — the batch nukes nothing", refused, len(targets))
	}
	text := out.String()
	if strings.Contains(text, "Would nuke ") {
		t.Errorf("preview promised a nuke for a batch that would be refused:\n%s", text)
	}
	if got := strings.Count(text, "Would refuse to nuke gastown/"); got != len(targets) {
		t.Errorf("refusal lines = %d, want %d:\n%s", got, len(targets), text)
	}
	for _, p := range targets {
		want := "Would refuse to nuke gastown/" + p.polecatName + " without --force:"
		if !strings.Contains(text, want) {
			t.Errorf("preview lacks %q:\n%s", want, text)
		}
	}
}

// A --force run does not stop at the safety refusal, but it still stops at
// preserveFailureBlocker when the self-preserve push fails: a target whose
// branch no remote ref holds is where that can cost the work, so the forced
// preview refuses it unless --acknowledge-unpreserved was given (gt-y6mhx).
func TestPreviewNukeForcedModelsPreserveRefusal(t *testing.T) {
	t.Parallel()
	reg := session.NewPrefixRegistry()
	reg.Register("zz", "gastown")
	target := nukePreviewTarget(t, reg, "basalt")
	clear := func(polecatTarget) *SafetyCheckResult { return &SafetyCheckResult{} }
	noList := func(polecatTarget, *SafetyCheckResult) {}

	var out bytes.Buffer
	refused := previewNukeDryRunBatch(&out, []polecatTarget{target}, true, reg, nukePreviewDeps{
		Check:      clear,
		ListSafety: noList,
		PreserveRisk: func(polecatTarget) nukePreviewRefusal {
			return nukePreviewRefusal{Clause: "under --force unless its self-preserve push succeeds"}
		},
	})
	if refused != 1 {
		t.Errorf("refused = %d, want 1 — the forced run is exposed to the preserve refusal", refused)
	}
	text := out.String()
	if !strings.Contains(text, "Would refuse to nuke gastown/basalt under --force") {
		t.Errorf("forced preview does not model the preserve refusal:\n%s", text)
	}

	// With --acknowledge-unpreserved the real run proceeds, so the preview does.
	out.Reset()
	refused = previewNukeDryRunBatch(&out, []polecatTarget{target}, true, reg, nukePreviewDeps{
		Check:        clear,
		ListSafety:   noList,
		PreserveRisk: func(polecatTarget) nukePreviewRefusal { return nukePreviewRefusal{} },
	})
	if refused != 0 || !strings.Contains(out.String(), "Would nuke gastown/basalt:") {
		t.Errorf("acknowledged forced preview = %d, %q; want a plain nuke", refused, out.String())
	}
}

// The forced nuke's second gate is preserveFailureBlocker, and the branch it
// guards is the one whose work no remote ref holds: a branch git reports as
// preserved (or with nothing unpreserved on it) leaves the nuke nothing to
// refuse (gt-y6mhx).
func TestUnpreservedBranchRefusalOnlyForUnpreservedWork(t *testing.T) {
	t.Parallel()
	target := polecatTarget{rigName: "gastown", polecatName: "basalt"}
	branch := "polecat/basalt/gt-y6mhx"

	for name, tc := range map[string]struct {
		status     git.BranchPreservationStatus
		wantClause bool
	}{
		"work no remote ref holds": {
			status:     git.BranchPreservationStatus{Preserved: false, UnpreservedPatchCount: 2},
			wantClause: true,
		},
		"preserved on a remote ref": {status: git.BranchPreservationStatus{Preserved: true}, wantClause: false},
		"nothing ahead of the remote": {
			status:     git.BranchPreservationStatus{Preserved: false, UnpreservedPatchCount: 0},
			wantClause: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			refusal := unpreservedBranchRefusalFor(target, branch, tc.status)
			if (refusal.Clause != "") != tc.wantClause {
				t.Fatalf("clause = %q, want clause present = %v", refusal.Clause, tc.wantClause)
			}
			if !tc.wantClause {
				return
			}
			for _, want := range []string{branch, NukeAcknowledgeUnpreservedFlag} {
				if !strings.Contains(strings.Join(refusal.Detail, "\n"), want) {
					t.Errorf("detail lacks %q:\n%s", want, strings.Join(refusal.Detail, "\n"))
				}
			}
		})
	}
}

// The preview used to judge a polecat twice — once for its header, once inside
// the safety listing — so the header, the listing and the "Would refuse N of M"
// count could disagree about the same seat. One evaluation per polecat, whose
// result the listing is handed (gt-y6mhx).
func TestPreviewNukeDryRunEvaluatesEachPolecatOnce(t *testing.T) {
	t.Parallel()
	reg := session.NewPrefixRegistry()
	reg.Register("zz", "gastown")
	targets := []polecatTarget{
		nukePreviewTarget(t, reg, "basalt"),
		nukePreviewTarget(t, reg, "flint"),
	}

	calls := make(map[string]int)
	listed := make(map[string]int)
	var out bytes.Buffer
	previewNukeDryRunBatch(&out, targets, false, reg, nukePreviewDeps{
		Check: func(p polecatTarget) *SafetyCheckResult {
			calls[p.polecatName]++
			return &SafetyCheckResult{}
		},
		ListSafety: func(p polecatTarget, _ *SafetyCheckResult) { listed[p.polecatName]++ },
	})

	for _, p := range targets {
		if calls[p.polecatName] != 1 {
			t.Errorf("checkPolecatSafety ran %d time(s) for %s, want 1", calls[p.polecatName], p.polecatName)
		}
		if listed[p.polecatName] != 1 {
			t.Errorf("safety listing ran %d time(s) for %s, want 1", listed[p.polecatName], p.polecatName)
		}
	}
}

// nukePreserveRepo looks for the branch where the nuke's own preserve step
// does: the seat's worktree first, the rig's bare repo second, nothing when
// neither exists (the case the nuke passes without pushing).
func TestNukePreserveRepoPicksWorktreeThenBareRepo(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "gastown", Path: t.TempDir()}

	if g := nukePreserveRepo(nil, r); g != nil {
		t.Errorf("nukePreserveRepo = %v with neither a worktree nor a bare repo, want nil", g)
	}

	bare := filepath.Join(r.Path, ".repo.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", bare, err)
	}
	if g := nukePreserveRepo(nil, r); g == nil {
		t.Error("nukePreserveRepo = nil with a bare repo present, want the bare repo")
	}

	worktree := t.TempDir()
	if g := nukePreserveRepo(&polecat.Polecat{ClonePath: worktree}, r); g == nil {
		t.Error("nukePreserveRepo = nil with a worktree present, want the worktree")
	} else if g.WorkDir() != worktree {
		t.Errorf("nukePreserveRepo work dir = %q, want the worktree %q", g.WorkDir(), worktree)
	}

	// A worktree path that does not exist is not a repo to read from: the bare
	// repo stands in, as it does for the nuke itself.
	if g := nukePreserveRepo(&polecat.Polecat{ClonePath: filepath.Join(r.Path, "gone")}, r); g == nil {
		t.Error("nukePreserveRepo = nil for a departed worktree with a bare repo present, want the bare repo")
	}
}
